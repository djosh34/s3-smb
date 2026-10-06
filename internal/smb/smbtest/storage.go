package smbtest

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/engine"
	"github.com/djosh34/s3-smb/internal/s3fault"
)

// NewStorage builds a real engine with its data folder in t.TempDir and its
// bucket in a local in-memory S3 server. Cleanup shuts the engine down. Close
// any server fixtures before this cleanup runs. Setup and cleanup errors are
// reported through t.
func NewStorage(t testing.TB) *engine.Engine {
	t.Helper()
	return newStorage(t, NewS3(t))
}

// NewS3Storage is NewStorage with the bucket behind the returned fault proxy,
// so proxy faults reach the engine as S3 delays, errors and outages.
func NewS3Storage(t testing.TB) (*engine.Engine, *s3fault.Proxy) {
	t.Helper()
	proxy, err := s3fault.New(t.Context(), NewS3(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return newStorage(t, proxy.URL()), proxy
}

func newStorage(t testing.TB, endpoint string) *engine.Engine {
	t.Helper()
	bucket, err := engine.NewBucket(engine.BucketOptions{
		Endpoint: endpoint, Region: "us-east-1", Bucket: "bucket",
		AccessKey: "test-access", SecretKey: "test-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Open(t.Context(), engine.Options{Bucket: bucket, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return e
}

// NewS3 starts an in-memory S3 server for the bucket named "bucket" and
// returns its URL. It knows path-style PUT, ranged GET, DELETE and
// ListObjectsV2, which is all the engine asks for. Cleanup stops it.
func NewS3(t testing.TB) string {
	t.Helper()
	s := &memS3{objects: make(map[string]memObject)}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	return server.URL
}

type memObject struct {
	modified time.Time
	data     []byte
}

type memS3 struct {
	objects map[string]memObject
	mu      sync.Mutex
}

func (s *memS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, "/bucket/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/bucket" && r.URL.Query().Get("list-type") == "2":
		s.list(w, r.URL.Query().Get("prefix"))
	case !ok || key == "":
		http.Error(w, "unsupported request", http.StatusBadRequest)
	case r.Method == http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.objects[key] = memObject{data: data, modified: time.Now().UTC()}
		s.mu.Unlock()
	case r.Method == http.MethodGet:
		s.get(w, r, key)
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported request", http.StatusBadRequest)
	}
}

func (s *memS3) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	object, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		reply(w, []byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
		return
	}
	data := object.data
	if spec := r.Header.Get("Range"); spec != "" {
		var first, last int
		if _, err := fmt.Sscanf(spec, "bytes=%d-%d", &first, &last); err != nil || first > last || first >= len(data) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		data = data[first:min(last+1, len(data))]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, first+len(data)-1, len(object.data)))
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	reply(w, data)
}

// reply writes a response body. It fails only when the client went away,
// which some tests cause on purpose.
func reply(w http.ResponseWriter, data []byte) {
	if _, err := w.Write(data); err != nil {
		log.Printf("in-memory S3: write response: %v", err)
	}
}

type listEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	Size         int    `xml:"Size"`
}

type listResult struct {
	XMLName     xml.Name    `xml:"ListBucketResult"`
	Name        string      `xml:"Name"`
	Contents    []listEntry `xml:"Contents"`
	IsTruncated bool        `xml:"IsTruncated"`
}

func (s *memS3) list(w http.ResponseWriter, prefix string) {
	result := listResult{Name: "bucket"}
	s.mu.Lock()
	for key, object := range s.objects {
		if strings.HasPrefix(key, prefix) {
			result.Contents = append(result.Contents, listEntry{Key: key, LastModified: object.modified.Format(time.RFC3339Nano), Size: len(object.data)})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(result.Contents, func(a, b listEntry) int { return strings.Compare(a.Key, b.Key) })
	w.Header().Set("Content-Type", "application/xml")
	data, err := xml.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reply(w, data)
}
