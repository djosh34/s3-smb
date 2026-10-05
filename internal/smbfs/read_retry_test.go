package smbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
)

func TestReadRetryRetriesTransientErrors(t *testing.T) {
	results := []syscall.Errno{syscall.EIO, syscall.EAGAIN, syscall.EIO, 0}
	calls := 0
	n, err := retryRead(t.Context(), func() (int, syscall.Errno) {
		calls++
		if results[calls-1] != 0 {
			return 0, results[calls-1]
		}
		return 7, 0
	}, readRetryWindow)
	if err != nil || n != 7 || calls != len(results) {
		t.Fatalf("read = %d, %v after %d calls", n, err, calls)
	}
}

func TestReadRetryReturnsOtherErrors(t *testing.T) {
	calls := 0
	n, err := retryRead(t.Context(), func() (int, syscall.Errno) {
		calls++
		return 2, syscall.EACCES
	}, readRetryWindow)
	if n != 2 || !errors.Is(err, syscall.EACCES) || calls != 1 {
		t.Fatalf("read = %d, %v after %d calls", n, err, calls)
	}
}

// A permanent failure ends with EIO when the window closes. The context
// deadline only stops the test if the window never ends.
func TestReadRetryStopsAtWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	calls := 0
	n, err := retryRead(ctx, func() (int, syscall.Errno) {
		calls++
		return 0, syscall.EIO
	}, readRetrySleep/2)
	requireError(t, err, smb.ErrIO)
	if n != 0 || !errors.Is(err, syscall.EIO) || errors.Is(err, context.DeadlineExceeded) || calls == 0 {
		t.Fatalf("permanent failure = %d, %v after %d calls", n, err, calls)
	}
}

func TestReadRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	_, err := retryRead(ctx, func() (int, syscall.Errno) {
		calls++
		return 0, syscall.EIO
	}, readRetryWindow)
	requireError(t, err, context.Canceled)
	if calls != 0 {
		t.Fatal("canceled read called the backend")
	}

	// The deadline passes while retryRead waits between attempts.
	ctx, cancel = context.WithTimeout(t.Context(), readRetrySleep/5)
	defer cancel()
	_, err = retryRead(ctx, func() (int, syscall.Errno) { return 0, syscall.EIO }, 10*time.Second)
	requireError(t, err, context.DeadlineExceeded)
}

// countedDataReader counts backend EIO results.
type countedDataReader struct {
	vfs.DataReader
	failures atomic.Int64
}

func (r *countedDataReader) Open(ino meta.Ino, size uint64) vfs.FileReader {
	return &countedFileReader{FileReader: r.DataReader.Open(ino, size), owner: r}
}

type countedFileReader struct {
	vfs.FileReader
	owner *countedDataReader
}

func (r *countedFileReader) Read(ctx meta.Context, off uint64, dst []byte) (int, syscall.Errno) {
	n, eno := r.FileReader.Read(ctx, off, dst)
	if eno == syscall.EIO {
		r.owner.failures.Add(1)
	}
	return n, eno
}

// A cold read across two blocks survives an S3 outage. The check gate uses the
// production outage length and retry budget.
func TestReadAcrossBlockBoundarySurvivesS3Outage(t *testing.T) {
	outage := 3 * time.Second
	gate := os.Getenv("S3_SMB_CHECK_MODE") == "gate"
	if gate {
		outage = smb.S3OutageWindow
	}
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	const blockSize = 64 << 10
	if f.config.Chunk.BlockSize != blockSize {
		t.Fatal("fixture block size changed")
	}
	const payload = "block boundary payload\n"
	data := bytes.Repeat([]byte(payload), 2*blockSize/len(payload)+1)
	if n, err := f.fs.WriteAt(t.Context(), h, data, 0); err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}

	// Serve the committed objects through the real S3 client. No cache or
	// read pages hold the data yet.
	backend := httptest.NewServer(http.StripPrefix("/bucket/", http.FileServer(http.Dir(filepath.Join(filepath.Dir(f.path), "objects")))))
	t.Cleanup(backend.Close)
	proxy, err := s3fault.New(t.Context(), backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	blob, err := object.CreateStorage("s3", proxy.URL()+"/bucket", "test-access", "test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	config := *f.config
	mc := *config.Meta
	if gate {
		mc.Retries = 53
	}
	config.Meta = &mc
	cc := *config.Chunk
	cc.GetTimeout = 500 * time.Millisecond
	config.Chunk = &cc
	reader := &countedDataReader{DataReader: vfs.NewDataReader(&config, f.metadata, chunk.NewCachedStore(blob, cc, nil))}
	f.fs.reader = reader
	ctx, cancel := context.WithTimeout(t.Context(), outage+3*time.Minute)
	t.Cleanup(cancel)
	t.Cleanup(proxy.RestoreS3)

	got := make([]byte, 32)
	offset := uint64(blockSize - 16)
	done := make(chan error, 1)
	proxy.FailS3For(outage)
	go func() {
		n, readErr := f.fs.ReadAt(ctx, h, got, offset)
		if readErr == nil && (n != len(got) || !bytes.Equal(got, data[offset:offset+uint64(len(got))])) {
			readErr = fmt.Errorf("cold read returned %d bytes with a data mismatch; want %d matching bytes", n, len(got))
		}
		done <- readErr
	}()
	paths := make(map[string]bool)
	for len(paths) < 2 {
		select {
		case event := <-proxy.OutageSeen():
			if event.Method != http.MethodGet || event.Status != http.StatusServiceUnavailable {
				t.Fatalf("unexpected outage request: %+v", event)
			}
			paths[event.Path] = true
		case readErr := <-done:
			t.Fatalf("read completed before both blocks reached the outage: %v", readErr)
		}
	}
	select {
	case readErr := <-done:
		if readErr != nil {
			t.Fatal(readErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if reader.failures.Load() == 0 {
		t.Fatal("outage did not exercise the adapter EIO retry")
	}
}
