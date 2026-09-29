// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// These are measurements, not a throughput promise or a supported-size ceiling.
func TestSMBMeasurements(t *testing.T) {
	f := newFixture(t, false)
	f.cacheSize = "8 MB"
	d := f.start()
	s, close := f.share()
	files := make(map[string][]byte)
	started := time.Now()
	for i := 0; i < 32; i++ {
		data := make([]byte, 65536)
		for j := range data {
			data[j] = byte(i + j*31 + j/257)
		}
		name := fmt.Sprintf("fixture-%02d.bin", i)
		files[name] = data
		writeFile(t, s, name, data)
	}
	t.Logf("32-file write+flush+close bytes=%d duration=%s", 32*65536, time.Since(started))
	close()
	f.protectedAfter(time.Now())
	d.stop()
	if err := os.RemoveAll(filepath.Join(f.root, "cache")); err != nil {
		t.Fatal(err)
	}
	d = f.start()
	s, close = f.share()
	for _, phase := range []string{"cold-after-cache-removal", "warm-same-daemon"} {
		started = time.Now()
		entries, err := s.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s listing entries=%d duration=%s", phase, len(entries), time.Since(started))
		started = time.Now()
		var read int
		for name, want := range files {
			got, err := s.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(got) != sha256.Sum256(want) {
				t.Fatalf("hash mismatch %s", name)
			}
			read += len(got)
		}
		t.Logf("%s full-hash read bytes=%d duration=%s", phase, read, time.Since(started))
	}
	close()
	d.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	objs, err := f.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, obj := range objs.Contents {
		if strings.Contains(aws.ToString(obj.Key), "meta/dump-") && aws.ToInt64(obj.Size) > size {
			size = aws.ToInt64(obj.Size)
		}
	}
	if size == 0 {
		t.Fatal("no export size measurement")
	}
	// Plaintext native gzip is uploaded whole: its length is also the completed
	// staging file's length, unlike a sampled in-progress size.
	t.Logf("largest metadata export completed_staging_gzip_bytes=%d; process peak RSS logged on each stop (includes ordinary runtime, not isolated export allocation)", size)
}
