// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// This deliberately loses a successful real MinIO PUT response, rather than
// fabricating a successful object-store result. It runs in the shared Docker
// unit phase with a disposable MinIO and no production credentials.
func TestMinIOBootstrapLostResponse(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("run scripts/test-linux.sh unit ./internal/storage")
	}
	upstream, err := url.Parse(endpoint)
	if err != nil || upstream.Scheme != "http" || upstream.Host == "" {
		t.Fatal("test requires disposable HTTP MinIO")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ready := false
	for !ready {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/minio/health/ready", nil)
		res, e := http.DefaultClient.Do(req)
		if e == nil {
			_ = res.Body.Close()
			ready = res.StatusCode == http.StatusOK
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("MinIO readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	var lost atomic.Bool
	var dropChunkResponses atomic.Bool
	var droppedChunks atomic.Int64
	var puts atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = func(res *http.Response) error {
		if res.Request.Method == http.MethodPut && strings.Contains(res.Request.URL.Path, "/chunks/") && dropChunkResponses.Load() && res.StatusCode >= 200 && res.StatusCode < 300 {
			droppedChunks.Add(1)
			_ = res.Body.Close()
			return errors.New("deliberately lost native chunk upload response")
		}
		if res.Request.Method == http.MethodPut && strings.Contains(res.Request.URL.Path, "/s3-smb/keys/") {
			puts.Add(1)
			if res.StatusCode >= 200 && res.StatusCode < 300 && lost.CompareAndSwap(false, true) {
				_ = res.Body.Close()
				return errors.New("deliberately lost committed response")
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Closing the connection without an HTTP response exercises an actual lost
		// network response. The upstream PUT has already returned success.
		c, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			_ = c.Close()
		}
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	raw, err := object.NewS3(object.S3Options{Bucket: fmt.Sprintf("bootstrap-%d", time.Now().UnixNano()), Region: "us-east-1", Endpoint: server.URL, AccessKey: "s3smb-test-access", SecretKey: "s3smb-test-secret-only"})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.(io.Closer).Close()
	if err = raw.Create(ctx); err != nil {
		t.Fatal(err)
	}
	format, err := NewFormat("s3-smb", true, 14)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := OpenVolume(ctx, raw, format, "synthetic-bootstrap-passphrase", true)
	if err != nil {
		t.Fatal("lost committed key response was not resolved by exact readback", err)
	}
	if !lost.Load() || puts.Load() != 1 {
		t.Fatalf("lost response was not exercised once: lost=%v puts=%d", lost.Load(), puts.Load())
	}
	keyPath := keyPrefix + format.UUID + ".pem"
	original, err := readBounded(ctx, raw, keyPath, maxKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateKeyEnvelope(original); err != nil {
		t.Fatal(err)
	}
	p := raw.(interface {
		PutIfAbsent(context.Context, string, io.Reader) error
	})
	if err = p.PutIfAbsent(ctx, keyPath, strings.NewReader("must never replace key")); err == nil {
		t.Fatal("MinIO overwrote existing key")
	}
	current, err := readBounded(ctx, raw, keyPath, maxKeyBytes)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("original key not preserved", err)
	}
	if err = PublishIdentity(ctx, raw, format); err != nil {
		t.Fatal(err)
	}
	if err = PublishMarker(ctx, blob, format); err != nil {
		t.Fatal(err)
	}
	data := []byte("fixture encrypted using the native data and bootstrap key")
	if err = blob.Put(ctx, "fixture", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Freshly read remote identity/key; no original key object is retained by the
	// restarted wrapper. The native parser and object decryption are real.
	saved, err := ReadIdentity(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenVolume(ctx, raw, saved, "synthetic-bootstrap-passphrase", false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readBounded(ctx, reopened, "fixture", 1024)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("fresh native wrapper failed data verification", err)
	}
	t.Run("zero_cache_upload_response_interrupted", func(t *testing.T) {
		zero := int64(0)
		c, e := CacheConfig(format, "/not-a-usable-cache", &zero)
		if e != nil {
			t.Fatal(e)
		}
		c.MaxRetries = 1
		store := chunk.NewCachedStore(blob, c, nil)
		completed := bytes.Repeat([]byte("completed-data"), 4096)
		w := store.NewWriter(101, 0)
		if _, e = w.WriteAt(completed, 0); e != nil {
			t.Fatal(e)
		}
		if e = w.Finish(len(completed)); e != nil {
			t.Fatal(e)
		}
		dropChunkResponses.Store(true)
		w = store.NewWriter(102, 0)
		if _, e = w.WriteAt(completed, 0); e != nil {
			t.Fatal(e)
		}
		e = w.Finish(len(completed))
		dropChunkResponses.Store(false)
		if e == nil {
			t.Fatal("interrupted S3 upload response reported successful zero-cache flush")
		}
		if droppedChunks.Load() == 0 {
			t.Fatal("network fault was not exercised")
		}
		cold := chunk.NewCachedStore(reopened, c, nil)
		page := chunk.NewOffPage(len(completed))
		defer page.Release()
		n, e := cold.NewReader(101, len(completed)).ReadAt(ctx, page, 0)
		if e != nil || n != len(completed) || !bytes.Equal(page.Data, completed) {
			t.Fatal("completed data did not survive interrupted upload and cold native reopen", n, e)
		}
		if cold.UsedMemory() != 0 {
			t.Fatal("zero cache retained data")
		}
	})
	before := puts.Load()
	if _, err = OpenVolume(ctx, raw, saved, "wrong-passphrase", false); err == nil {
		t.Fatal("wrong passphrase succeeded")
	}
	if puts.Load() != before {
		t.Fatal("wrong passphrase mutated key")
	}
	if err = raw.Put(ctx, keyPath, strings.NewReader("deliberately corrupt protected key")); err != nil {
		t.Fatal(err)
	}
	before = puts.Load()
	if _, err = OpenVolume(ctx, raw, saved, "synthetic-bootstrap-passphrase", false); err == nil {
		t.Fatal("corrupt key succeeded")
	}
	if puts.Load() != before {
		t.Fatal("corrupt key regenerated")
	}
	if err = raw.Delete(ctx, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, saved, "synthetic-bootstrap-passphrase", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
	if puts.Load() != before {
		t.Fatal("missing key regenerated")
	}
	t.Log("real MinIO committed key PUT response lost, exact readback recovered; conditional collision preserved key; wrong/corrupt/missing key failed without mutation")
}
