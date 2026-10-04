package smbfs

import (
	"bytes"
	"context"
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

type observedDataReader struct {
	vfs.DataReader
	failures atomic.Int64
}

func (r *observedDataReader) Open(ino meta.Ino, size uint64) vfs.FileReader {
	return &observedFileReader{FileReader: r.DataReader.Open(ino, size), owner: r}
}

type observedFileReader struct {
	vfs.FileReader
	owner *observedDataReader
}

func (r *observedFileReader) Read(ctx meta.Context, off uint64, dst []byte) (int, syscall.Errno) {
	n, eno := r.FileReader.Read(ctx, off, dst)
	if eno == syscall.EIO {
		r.owner.failures.Add(1)
	}
	return n, eno
}

func TestReadAcrossBlockBoundarySurvivesS3Outage(t *testing.T) {
	outage := 3 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = 300 * time.Second
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

	// Serve the committed file-backed objects through the real S3 GET client.
	// FileServer supplies the byte ranges used by JuiceFS; no chunk cache or
	// VFS pages have been populated by a read.
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
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		mc.Retries = 53 // Match the production per-file retry budget at the gate.
	}
	config.Meta = &mc
	cc := *config.Chunk
	cc.GetTimeout = 500 * time.Millisecond
	config.Chunk = &cc
	reader := &observedDataReader{DataReader: vfs.NewDataReader(&config, f.metadata, chunk.NewCachedStore(blob, cc, nil))}
	f.fs.reader = reader
	ctx, cancel := context.WithTimeout(t.Context(), outage+3*time.Minute)
	t.Cleanup(cancel)
	t.Cleanup(proxy.RestoreS3)
	got := make([]byte, 32)
	offset := uint64(blockSize - 16)
	done := make(chan error, 1)
	start := proxy.FailS3For(outage)
	go func() {
		n, readErr := f.fs.ReadAt(ctx, h, got, offset)
		if readErr == nil && (n != len(got) || !bytes.Equal(got, data[offset:offset+uint64(len(got))])) {
			readErr = smb.ErrIO
		}
		done <- readErr
	}()
	paths := make(map[string]bool)
	firstSecond := time.NewTimer(time.Second)
	defer firstSecond.Stop()
	for len(paths) < 2 {
		select {
		case event := <-proxy.OutageSeen():
			if event.Method != http.MethodGet || event.Status != http.StatusServiceUnavailable || time.Since(start) >= time.Second {
				t.Fatalf("unexpected outage request: %+v after %s", event, time.Since(start))
			}
			paths[event.Path] = true
		case readErr := <-done:
			t.Fatalf("read completed before both blocks reached the outage: %v", readErr)
		case <-firstSecond.C:
			t.Fatal("both blocks did not reach failed S3 in the first second")
		}
	}
	select {
	case readErr := <-done:
		if readErr != nil {
			t.Fatal(readErr)
		}
		if time.Since(start) < outage {
			t.Fatal("cold read completed before S3 recovered")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if reader.failures.Load() == 0 {
		t.Fatal("outage did not exercise the adapter EIO retry")
	}
	t.Logf("read survived %s outage after %d backend EIOs", outage, reader.failures.Load())
}
