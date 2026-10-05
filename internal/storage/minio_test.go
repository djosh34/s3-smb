// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// minioEndpoint returns the disposable MinIO that scripts/check.sh starts, once
// it is ready. Without one the test is skipped.
func minioEndpoint(t *testing.T) *url.URL {
	t.Helper()
	value := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if value == "" {
		t.Skip("needs MinIO: run scripts/check.sh")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		t.Fatal("S3_SMB_E2E_ENDPOINT must be the HTTP URL of a disposable MinIO")
	}
	endpoint := &url.URL{Scheme: "http", Host: parsed.Host}
	waitMinIO(t, endpoint)
	return endpoint
}

// minioCredentials returns the root account of the disposable MinIO.
func minioCredentials() (access, secret string) {
	return "s3smb-test-access", "s3smb-test-secret-only"
}

func waitMinIO(t *testing.T, endpoint *url.URL) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	ready := endpoint.JoinPath("minio", "health", "ready").String()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, ready, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			if err = response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("MinIO did not become ready")
		case <-tick.C:
		}
	}
}

// minioBucket creates a new bucket named after prefix behind endpoint.
func minioBucket(ctx context.Context, t *testing.T, endpoint, prefix string) object.ObjectStorage {
	t.Helper()
	access, secret := minioCredentials()
	raw, err := object.NewS3(object.S3Options{Bucket: fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()), Region: "us-east-1", Endpoint: endpoint, AccessKey: access, SecretKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := raw.(io.Closer); ok {
		t.Cleanup(func() {
			if closeErr := closer.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err = raw.Create(ctx); err != nil {
		t.Fatal(err)
	}
	return raw
}

// lossyProxy forwards to upstream. It drops the response to the first
// successful key PUT by closing the connection after MinIO has committed it.
func lossyProxy(t *testing.T, upstream *url.URL, lost *atomic.Bool, puts *atomic.Int64) *httptest.Server {
	t.Helper()
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = func(res *http.Response) error {
		if res.Request.Method != http.MethodPut || !strings.Contains(res.Request.URL.Path, "/"+keyPrefix) {
			return nil
		}
		puts.Add(1)
		if res.StatusCode < 200 || res.StatusCode >= 300 || !lost.CompareAndSwap(false, true) {
			return nil
		}
		return errors.Join(errors.New("deliberately lost committed response"), res.Body.Close())
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			err = conn.Close()
		}
		if err != nil {
			t.Error(err)
		}
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server
}

func TestMinIOBootstrapLostResponse(t *testing.T) {
	upstream := minioEndpoint(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var lost atomic.Bool
	var puts atomic.Int64
	raw := minioBucket(ctx, t, lossyProxy(t, upstream, &lost, &puts).URL, "bootstrap")
	format, err := NewFormat(VolumeName, true, 14)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := OpenVolume(ctx, raw, format, testPassphrase, true)
	if err != nil {
		t.Fatal("lost committed key response was not resolved by the readback", err)
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
	if err = publishExact(ctx, raw, keyPath, []byte("must never replace key")); err == nil {
		t.Fatal("MinIO overwrote the existing key")
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
	data := []byte("fixture encrypted with the bootstrap key")
	if err = blob.Put(ctx, "fixture", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// A restart reads the identity and key back from the bucket.
	saved, err := ReadIdentity(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenVolume(ctx, raw, saved, testPassphrase, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readBounded(ctx, reopened, "fixture", 1024)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("reopened volume failed data verification", err)
	}
}

func TestMinIOCacheEvictionRefetch(t *testing.T) {
	endpoint := minioEndpoint(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	raw := minioBucket(ctx, t, endpoint.String(), "cache-eviction")
	format, err := NewFormat("cache-eviction", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	const payloadSize = 1 << 20
	capacity := uint64(2 << 20)
	conf, err := CacheConfig(format, t.TempDir(), &capacity)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countStore{ObjectStorage: raw}
	registry := prometheus.NewRegistry()
	store := chunk.NewCachedStore(counted, conf, registry)
	cache := diskCache{store: store, dir: conf.CacheDir, capacity: int64(capacity), block: payloadSize}

	// Each partial block is cached on write. Finish waits for its S3 upload.
	// Eight blocks exceed both the capacity and the flush slack.
	data := make([][]byte, 8)
	for i := range data {
		data[i] = make([]byte, payloadSize)
		for offset := range data[i] {
			data[i][offset] = byte((offset*31 + offset/251 + i*17) % 256)
		}
		writer := store.NewWriter(uint64(i+1), 0)
		if n, writeErr := writer.WriteAt(data[i], 0); writeErr != nil || n != payloadSize {
			t.Fatalf("write slice %d: bytes=%d err=%v", i+1, n, writeErr)
		}
		if err = writer.Finish(payloadSize); err != nil {
			t.Fatalf("flush slice %d: %v", i+1, err)
		}
		cache.settle(ctx, t)
	}
	if evictions := counterTotal(t, registry, "blockcache_evicts"); evictions == 0 {
		t.Fatal("dataset did not force disk cache eviction")
	}

	// Keep the same store and directory. Evicted slices must come from MinIO,
	// not a fresh cache or a retained page.
	refetched := 0
	for i, want := range data {
		id := uint64(i + 1)
		cached := cache.holds(t, id)
		before := counted.gets.Load()
		got, readErr := readSlice(store, id, payloadSize)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("slice %d changed: %v", id, readErr)
		}
		if cached && counted.gets.Load() != before {
			t.Fatalf("cached slice %d was fetched from S3", id)
		}
		if !cached {
			if counted.gets.Load() <= before {
				t.Fatalf("evicted slice %d was not refetched from S3", id)
			}
			refetched++
		}
		cache.settle(ctx, t)
	}
	if refetched == 0 {
		t.Fatal("no evicted data was read back")
	}
}

func counterTotal(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, metric := range metrics {
		if metric.GetName() == name {
			for _, sample := range metric.GetMetric() {
				total += sample.GetCounter().GetValue()
			}
		}
	}
	return total
}

type diskCache struct {
	store    chunk.ChunkStore
	dir      string
	capacity int64
	block    uint32
}

func (c diskCache) holds(t *testing.T, id uint64) bool {
	t.Helper()
	cached := false
	if err := c.store.CheckCache(id, c.block, func(exists bool, _ string, _ int) { cached = exists }); err != nil {
		t.Fatal(err)
	}
	return cached
}

// settle waits until JuiceFS has written its pending pages to the disk cache
// and checks the directory size on the way. disk_cache.go writes one block
// before it evicts down to 95% of capacity, so one block of slack is allowed
// while pages are pending. Locks and checksums may use another 4 KiB.
func (c diskCache) settle(ctx context.Context, t *testing.T) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		pending := c.store.UsedMemory()
		size := c.size(t)
		if size > c.capacity+int64(c.block)+4096 {
			t.Fatalf("cache directory uses %d bytes, more than %d bytes with flush slack", size, c.capacity+int64(c.block)+4096)
		}
		if pending == 0 {
			if size > c.capacity+4096 {
				t.Fatalf("settled cache directory uses %d bytes, more than %d bytes", size, c.capacity+4096)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("disk cache did not settle: pending=%d bytes, directory=%d bytes", pending, size)
		case <-tick.C:
		}
	}
}

// size counts all regular files, including checksums and temporary files.
func (c diskCache) size(t *testing.T) int64 {
	t.Helper()
	var size int64
	err := filepath.WalkDir(c.dir, func(_ string, entry fs.DirEntry, err error) error {
		// An eviction can remove a file during the walk.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return size
}
