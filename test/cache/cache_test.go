// SPDX-License-Identifier: AGPL-3.0-only
package cache_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/storage"
	"github.com/google/uuid"
)

const (
	fileCount = 6
	fileSize  = 4 << 20
	capacity  = int64(5_000_000)
)

// TestMinIOPositiveDiskCache is deliberately distinct from the SMB zero-cache
// and encrypted recovery tests. It checks the native retained disk cache under
// pressure, using real MinIO and a new process for each cold-read phase.
func TestMinIOPositiveDiskCache(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires shared disposable MinIO: scripts/test-linux.sh unit ./test/cache")
	}
	upstream, err := url.Parse(endpoint)
	if err != nil || upstream.Scheme != "http" || upstream.Host == "" {
		t.Fatal("expected shared disposable HTTP MinIO endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// This observer does not serve fake S3 data or modify the signed Host/path.
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var mu sync.Mutex
	gets := make(map[string]int)
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == http.MethodGet && strings.Contains(r.Request.URL.Path, "/chunks/") && (r.StatusCode == 200 || r.StatusCode == 206) {
			mu.Lock()
			gets[r.Request.URL.Path]++
			mu.Unlock()
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	state := t.TempDir()
	bucket := "cache-" + uuid.NewString()
	worker := func(phase string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCacheWorker$", "-test.v")
		cmd.Env = append(os.Environ(), "S3_SMB_CACHE_PHASE="+phase, "S3_SMB_CACHE_STATE="+state, "S3_SMB_CACHE_ENDPOINT="+server.URL, "S3_SMB_CACHE_BUCKET="+bucket, "GOMAXPROCS=2")
		out, err := cmd.CombinedOutput()
		t.Logf("native cache phase %s:\n%s", phase, out)
		if err != nil {
			t.Fatalf("native cache phase %s: %v", phase, err)
		}
	}
	resetGets := func() {
		mu.Lock()
		gets = make(map[string]int)
		mu.Unlock()
	}
	assertGets := func(repeated bool) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		total, refetched := 0, false
		for _, n := range gets {
			total += n
			refetched = refetched || n > 1
		}
		if len(gets) != fileCount || (repeated && !refetched) {
			t.Fatalf("S3 data GET evidence: %d distinct objects, %d requests, repeat=%v; want %d objects, repeat required=%v", len(gets), total, refetched, fileCount, repeated)
		}
		t.Logf("MinIO chunk GET evidence: %d distinct objects, %d successful GETs, repeated_object_GET=%v", len(gets), total, refetched)
	}
	worker("write")
	// Remove only disposable cache, never SQLite or data, after its owning
	// process exits. The next process has no active reader or RAM cache.
	if err := os.RemoveAll(filepath.Join(state, "cache")); err != nil {
		t.Fatal(err)
	}
	resetGets()
	worker("evict")
	assertGets(true)
	if err := os.RemoveAll(filepath.Join(state, "cache")); err != nil {
		t.Fatal(err)
	}
	resetGets()
	worker("cold")
	assertGets(false)
}

// TestMinIOZeroCacheIgnoresPopulatedDiskCache keeps one real populated cache
// tree across positive-to-zero restarts. A valid old cache cannot rescue a
// zero-cache read when remote data GETs are denied.
func TestMinIOZeroCacheIgnoresPopulatedDiskCache(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires shared disposable MinIO: scripts/test-linux.sh unit ./test/cache")
	}
	upstream, err := url.Parse(endpoint)
	if err != nil || upstream.Scheme != "http" || upstream.Host == "" {
		t.Fatal("expected shared disposable HTTP MinIO endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var mu sync.Mutex
	var deny bool
	var denied, gets int
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == http.MethodGet && strings.Contains(r.Request.URL.Path, "/chunks/") && (r.StatusCode == 200 || r.StatusCode == 206) {
			mu.Lock()
			gets++
			mu.Unlock()
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		blocked := deny && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/chunks/")
		if blocked {
			denied++
		}
		mu.Unlock()
		if blocked {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>cache acceptance injected denial</Message></Error>`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	state := t.TempDir()
	bucket := "cache-zero-" + uuid.NewString()
	worker := func(phase string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCacheWorker$", "-test.v")
		cmd.Env = append(os.Environ(), "S3_SMB_CACHE_PHASE="+phase, "S3_SMB_CACHE_SINGLE=1", "S3_SMB_CACHE_STATE="+state, "S3_SMB_CACHE_ENDPOINT="+server.URL, "S3_SMB_CACHE_BUCKET="+bucket, "GOMAXPROCS=2")
		out, err := cmd.CombinedOutput()
		t.Logf("retained-cache phase %s:\n%s", phase, out)
		if err != nil {
			t.Fatalf("retained-cache phase %s: %v", phase, err)
		}
	}
	worker("write")
	worker("cold") // Positive capacity populates native retained disk cache.
	mu.Lock()
	deny, denied, gets = true, 0, 0
	mu.Unlock()
	worker("cold") // Prove that exact old cache is valid even without remote GETs.
	mu.Lock()
	warmDenied, warmGets := denied, gets
	mu.Unlock()
	if warmDenied != 0 || warmGets != 0 {
		t.Fatalf("valid positive cache unexpectedly requested remote data: denied=%d successful=%d", warmDenied, warmGets)
	}
	oldTree := retainedCacheTree(t, filepath.Join(state, "cache"))
	t.Log("valid populated native disk cache survived positive-process exit; no remote GET needed")
	worker("zero-denied")
	mu.Lock()
	blocked := denied
	deny, gets = false, 0
	mu.Unlock()
	if blocked == 0 {
		t.Fatal("zero-cache denied read never attempted remote data GET")
	}
	if !maps.Equal(oldTree, retainedCacheTree(t, filepath.Join(state, "cache"))) {
		t.Fatal("explicit zero modified or replaced the retained positive cache tree")
	}
	worker("zero-cold")
	mu.Lock()
	fetched := gets
	mu.Unlock()
	if fetched == 0 {
		t.Fatal("zero-cache successful full read did not fetch MinIO data")
	}
	if !maps.Equal(oldTree, retainedCacheTree(t, filepath.Join(state, "cache"))) {
		t.Fatal("explicit zero changed old cache after successful remote read")
	}
	t.Logf("unchanged populated cache tree: explicit-zero denial failed after %d attempted GETs; allowed restart verified full hash with %d real MinIO GET responses", blocked, fetched)
}

// A subprocess entry point, not an independently skipped acceptance case.
func TestCacheWorker(t *testing.T) {
	phase := os.Getenv("S3_SMB_CACHE_PHASE")
	if phase == "" {
		return
	}
	state := os.Getenv("S3_SMB_CACHE_STATE")
	if state == "" {
		t.Fatal("missing private cache-test state directory")
	}
	count := fileCount
	if os.Getenv("S3_SMB_CACHE_SINGLE") == "1" {
		count = 1
	}
	ctx := context.Background()
	pathStyle := true
	raw, err := storage.OpenS3(&config.Resolved{
		Config:    &config.Config{S3: config.S3Config{Bucket: os.Getenv("S3_SMB_CACHE_BUCKET"), Region: "us-east-1", Endpoint: os.Getenv("S3_SMB_CACHE_ENDPOINT"), PathStyle: &pathStyle}},
		AccessKey: "s3smb-test-access", SecretKey: "s3smb-test-secret-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	var format *meta.Format
	if phase == "write" {
		// The fixture starts MinIO concurrently with the runner. Retry only its
		// readiness, bounded; subsequent operations retain ordinary native retries.
		deadline := time.Now().Add(30 * time.Second)
		for {
			err = raw.Create(ctx)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("MinIO bucket creation: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		format, err = storage.NewFormat("cache-acceptance", false, 14)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(format)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "format.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		data, err := os.ReadFile(filepath.Join(state, "format.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &format); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := storage.OpenVolume(ctx, raw, format, "", phase == "write")
	if err != nil {
		t.Fatal(err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true // This test exercises cache I/O, not protection/GC.
	m, err := storage.OpenMetadata(filepath.Join(state, "metadata.db"), conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	if phase == "write" {
		if err := m.Init(format, false); err != nil {
			t.Fatal(err)
		}
	} else {
		loaded, err := m.Load(false)
		if err != nil || loaded.UUID != format.UUID {
			t.Fatalf("native metadata reopen: %v", err)
		}
	}
	if err := m.NewSession(false); err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseByteSize("5 MB")
	if err != nil || int64(parsed) != capacity {
		t.Fatalf("decimal cache input %d: %v", parsed, err)
	}
	cacheBytes := int64(parsed)
	zero := strings.HasPrefix(phase, "zero-")
	if zero {
		cacheBytes = 0
	}
	cacheDir := filepath.Join(state, "cache") // Intentionally unchanged for zero.
	nativeConf, err := storage.CacheConfig(format, cacheDir, &cacheBytes)
	if err != nil || nativeConf.CacheSize != uint64(cacheBytes) || (nativeConf.CacheDir == "memory") != zero {
		t.Fatalf("explicit capacity did not reach native cache configuration: %v", err)
	}
	runtime, err := storage.OpenFilesystem(m, blob, format, cacheDir, &cacheBytes, func() error { return fmt.Errorf("cache acceptance never permits destructive maintenance") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	mctx := meta.NewContext(1, 0, []uint32{0})
	switch phase {
	case "write":
		for i := 0; i < count; i++ {
			f, errno := runtime.FS.Create(mctx, fixtureName(i), 0600, 0)
			if errno != 0 {
				t.Fatal(errno)
			}
			data := fixture(i)
			if n, errno := f.Write(mctx, data); errno != 0 || n != len(data) {
				t.Fatal(n, errno)
			}
			if errno := f.Fsync(mctx); errno != 0 {
				t.Fatal(errno)
			}
			if errno := f.Close(mctx); errno != 0 {
				t.Fatal(errno)
			}
		}
		// Native full-block writes do not retain a cache entry unless
		// CacheLargeWrite is enabled. Cache acceptance starts with reads.
		t.Logf("wrote and fsynced %d files / %d bytes; retained disk capacity=%d bytes", count, count*fileSize, cacheBytes)
	case "evict":
		slices := make([]meta.Slice, fileCount)
		for i := range slices {
			var ino meta.Ino
			var attr meta.Attr
			if errno := m.Lookup(mctx, meta.RootInode, strings.TrimPrefix(fixtureName(i), "/"), &ino, &attr, true); errno != 0 {
				t.Fatal(errno)
			}
			var refs []meta.Slice
			if errno := m.Read(mctx, ino, 0, &refs); errno != 0 {
				t.Fatal(errno)
			}
			if len(refs) != 1 || refs[0].Size != fileSize || refs[0].Off != 0 || refs[0].Len != fileSize {
				t.Fatalf("unexpected fixture slices: %+v", refs)
			}
			slices[i] = refs[0]
			readSlice(t, runtime.Store, refs[0], i)
			waitDisk(t, runtime.Store, cacheDir)
			if !cached(t, runtime.Store, refs[0]) {
				t.Fatalf("fixture %d never populated native retained disk cache", i)
			}
			// Native 2-random compares whole-second atimes. Separate reads by
			// that clock tick so two-block pressure evicts the older block,
			// rather than asserting an ordering for equal-age candidates.
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)) + 20*time.Millisecond)
		}
		// Each object was present after its read; pressure must have evicted
		// at least one. Reopen a native chunk reader to avoid active FS buffers.
		evicted := -1
		for i, ref := range slices {
			if !cached(t, runtime.Store, ref) {
				evicted = i
				break
			}
		}
		if evicted < 0 {
			t.Fatal("dataset exceeds capacity but native cache did not evict")
		}
		readSlice(t, runtime.Store, slices[evicted], evicted)
		waitDisk(t, runtime.Store, cacheDir)
		if !cached(t, runtime.Store, slices[evicted]) {
			t.Fatal("evicted object was not cached after refetch")
		}
		t.Logf("native pressure evicted file %d / slice %d; refetch SHA-256 verified", evicted, slices[evicted].Id)
	case "zero-denied":
		f, errno := runtime.FS.Open(mctx, fixtureName(0), 0)
		if errno != 0 {
			t.Fatal(errno)
		}
		n, readErr := f.Read(mctx, make([]byte, fileSize))
		if errno := f.Close(mctx); errno != 0 {
			t.Fatal(errno)
		}
		if readErr == nil || readErr == io.EOF || n != 0 {
			t.Fatalf("zero-cache read served old cached data or empty success despite remote denial: n=%d err=%v", n, readErr)
		}
		if runtime.Store.UsedMemory() != 0 {
			t.Fatal("explicit zero retained memory cache")
		}
		t.Logf("zero-cache native FS read correctly failed despite valid retained disk cache: %v", readErr)
	case "cold", "zero-cold":
		for i := 0; i < count; i++ {
			f, errno := runtime.FS.Open(mctx, fixtureName(i), 0)
			if errno != 0 {
				t.Fatal(errno)
			}
			h := sha256.New()
			buf, total := make([]byte, 128<<10), 0
			for {
				n, err := f.Read(mctx, buf)
				_, _ = h.Write(buf[:n])
				total += n
				if err == io.EOF {
					break
				}
				if err != nil || n == 0 {
					t.Fatalf("cold file %d read: %d %v", i, n, err)
				}
			}
			if errno := f.Close(mctx); errno != 0 {
				t.Fatal(errno)
			}
			want := sha256.Sum256(fixture(i))
			if total != fileSize || string(h.Sum(nil)) != string(want[:]) {
				t.Fatalf("cold file %d hash/length mismatch", i)
			}
			t.Logf("cold restart complete file %d: %d bytes sha256=%x", i, total, want)
		}
		if zero {
			if runtime.Store.UsedMemory() != 0 {
				t.Fatal("explicit zero retained memory cache after remote read")
			}
		} else {
			waitDisk(t, runtime.Store, cacheDir)
		}
	default:
		t.Fatal("unknown cache worker phase")
	}
}

// Snapshot only observes the old cache; zero-mode phases never delete, chmod,
// replace or move it. Contents, names, modes, sizes and mtimes must survive.
func retainedCacheTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	blocks := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var sum [32]byte
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum = sha256.Sum256(data)
			if strings.Contains(filepath.ToSlash(path), "/raw/chunks/") {
				if info.Size() < fileSize {
					return fmt.Errorf("retained native block is shorter than fixture")
				}
				blocks++
			}
		}
		tree[rel] = fmt.Sprintf("%v:%d:%d:%x", info.Mode(), info.Size(), info.ModTime().UnixNano(), sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if blocks != 1 {
		t.Fatalf("expected one real populated native disk-cache block, found %d", blocks)
	}
	return tree
}

func fixtureName(i int) string { return fmt.Sprintf("/fixture-%02d", i) }
func fixture(i int) []byte {
	b := make([]byte, fileSize)
	_, _ = rand.New(rand.NewSource(int64(100 + i))).Read(b)
	return b
}
func readSlice(t *testing.T, store chunk.ChunkStore, ref meta.Slice, i int) {
	t.Helper()
	page := chunk.NewOffPage(fileSize)
	defer page.Release()
	n, err := store.NewReader(ref.Id, int(ref.Size)).ReadAt(context.Background(), page, 0)
	if err != nil || n != fileSize || sha256.Sum256(page.Data) != sha256.Sum256(fixture(i)) {
		t.Fatalf("native slice %d full read/hash: %d %v", ref.Id, n, err)
	}
}
func cached(t *testing.T, store chunk.ChunkStore, ref meta.Slice) bool {
	t.Helper()
	found := false
	if err := store.CheckCache(ref.Id, ref.Size, func(exists bool, _ string, _ int) { found = exists }); err != nil {
		t.Fatal(err)
	}
	return found
}
func waitDisk(t *testing.T, store chunk.ChunkStore, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var bytes int64
	var blocks int
	for {
		bytes, blocks = 0, 0
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.Contains(filepath.ToSlash(path), "/raw/chunks/") || strings.HasSuffix(path, ".tmp") {
				return nil
			}
			info, err := d.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			bytes += info.Size()
			blocks++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// Native disk accounting includes 4096 bytes of per-block overhead.
		if blocks > 0 && bytes+int64(blocks)*4096 <= capacity && store.UsedMemory() == 0 {
			t.Logf("native disk cache settled: %d blocks, %d file bytes, bounded below %d-byte capacity including per-block overhead", blocks, bytes, capacity)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("native disk cache did not settle: blocks=%d bytes=%d buffered=%d capacity=%d", blocks, bytes, store.UsedMemory(), capacity)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
