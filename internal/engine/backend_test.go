// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
)

// realOptions selects B2 when B2_BUCKET is set, as in the by-hand B2
// workflow, and otherwise the disposable MinIO that scripts/check.sh starts.
// Without either the test is skipped.
func realOptions(t *testing.T) (BucketOptions, bool) {
	t.Helper()
	if bucket := os.Getenv("B2_BUCKET"); bucket != "" {
		endpoint := os.Getenv("B2_ENDPOINT")
		// The endpoint looks like https://s3.<region>.backblazeb2.com.
		parts := strings.Split(strings.TrimPrefix(endpoint, "https://"), ".")
		if len(parts) < 2 {
			t.Fatalf("cannot read the region from B2_ENDPOINT %q", endpoint)
		}
		return BucketOptions{
			Endpoint: endpoint, Region: parts[1], Bucket: bucket, PathStyle: true,
			AccessKey: os.Getenv("B2_KEY_ID"), SecretKey: os.Getenv("B2_APPLICATION_KEY"),
		}, true
	}
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("needs MinIO or B2: run scripts/check.sh")
	}
	access, secret := minioCredentials()
	return BucketOptions{
		Endpoint: endpoint, Region: "us-east-1", Bucket: fmt.Sprintf("engine-%d", time.Now().UnixNano()), PathStyle: true,
		AccessKey: access, SecretKey: secret,
	}, false
}

// minioCredentials returns the root account of the disposable MinIO.
func minioCredentials() (access, secret string) {
	return "s3smb-test-access", "s3smb-test-secret-only"
}

// realBucket returns a bucket under a prefix of its own. On MinIO it creates
// the bucket. Cleanup deletes every object version under the prefix by ID
// and fails if any is left.
func realBucket(t *testing.T, options BucketOptions, b2 bool, maxBackoff time.Duration) *Bucket {
	t.Helper()
	b, err := newBucket(options, maxBackoff)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !b2 {
		if _, err = b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(options.Bucket)}); err != nil {
			t.Fatal(err)
		}
	}
	run, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	b.prefix = "engine-test/" + run + "/"
	t.Cleanup(func() { deleteVersions(t, b) })
	return b
}

func deleteVersions(t *testing.T, b *Bucket) {
	t.Helper()
	ctx := context.Background()
	for range 3 {
		var left int
		pages := s3.NewListObjectVersionsPaginator(b.client, &s3.ListObjectVersionsInput{Bucket: aws.String(b.name), Prefix: aws.String(b.prefix)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ids [][2]*string
			for _, v := range page.Versions {
				ids = append(ids, [2]*string{v.Key, v.VersionId})
			}
			for _, m := range page.DeleteMarkers {
				ids = append(ids, [2]*string{m.Key, m.VersionId})
			}
			left += len(ids)
			for _, id := range ids {
				if _, err = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.name), Key: id[0], VersionId: id[1]}); err != nil {
					t.Error(err)
				}
			}
		}
		if left == 0 {
			return
		}
	}
	t.Errorf("object versions are left under %s", b.prefix)
}

func openReal(t *testing.T, b *Bucket, dir string, tune tuning) *Engine {
	t.Helper()
	e, err := open(t.Context(), Options{Dir: dir}, b, tune)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kill(t, e) })
	return e
}

// realTuning keeps the fixed rules but uses 64 KiB chunks, so a few hundred
// KiB cross chunks and upload early.
func realTuning() tuning {
	tune := defaultTuning()
	tune.chunkSize = 64 << 10
	tune.dirtyChunks = 4
	tune.stale = 0
	return tune
}

// The engine against a real S3 store: early uploads, ranged reads, the
// namespace, trash deletes, a restart, a lost disk and a clean exit.
func TestRealBackend(t *testing.T) {
	options, b2 := realOptions(t)
	b := realBucket(t, options, b2, retry.DefaultMaxBackoff)
	tune := realTuning()
	dir := t.TempDir()
	e := openReal(t, b, dir, tune)
	big := make([]byte, 320<<10)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	create(t, e, "d", smb.KindDirectory)
	create(t, e, "d/big", smb.KindFile)
	h := openFile(t, e, "d/big", smb.AccessRead|smb.AccessWrite)
	for offset := 0; offset < len(big); offset += 32 << 10 {
		writeAt(t, e, h, string(big[offset:offset+32<<10]), uint64(offset))
	}
	flush(t, e, h)
	part := make([]byte, 100<<10)
	if n, err := e.ReadAt(t.Context(), h, part, 50<<10); err != nil || !bytes.Equal(part[:n], big[50<<10:150<<10]) {
		t.Fatalf("ranged read %d, %v", n, err)
	}
	if err := e.Truncate(t.Context(), h, 100<<10); err != nil {
		t.Fatal(err)
	}
	closeFile(t, e, h)
	writeFile(t, e, "small", "short")
	moved := lookup(t, e, "d/big")
	destination := lookup(t, e, "d")
	err := e.Rename(t.Context(), smb.RenameRequest{
		Source: moved.Name, SourceInode: moved.Object,
		Destination: smb.Name{Parent: destination.Object, Base: "moved"},
	})
	if err != nil {
		t.Fatal(err)
	}
	small := lookup(t, e, "small")
	if err = e.Remove(t.Context(), small.Name, small.Object); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"d/": "", "d/moved": string(big[:100<<10])}
	requireTree(t, e, want)
	for range 5 {
		copyNow(t, e)
	}
	if n := countRows(t, e, "trash"); n != 0 {
		t.Fatalf("%d trash rows after five copies", n)
	}
	chunks, err := b.list(t.Context(), chunkPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, e, "chunks"); len(chunks) != n {
		t.Fatalf("%d chunk objects for %d chunk rows", len(chunks), n)
	}
	checkCopies(t, b)
	shutdown(t, e)
	if keys, err := b.list(t.Context(), lockPrefix); err != nil || len(keys) != 0 {
		t.Fatalf("lock keys after a clean exit: %v, %v", keys, err)
	}

	e = openReal(t, b, dir, tune)
	requireTree(t, e, want)
	kill(t, e)
	e = openReal(t, b, t.TempDir(), tune)
	requireTree(t, e, want)
	shutdown(t, e)
}

// A 5-minute outage that starts just after a renewal, scaled down by 60,
// through the fault proxy and the real client: each request retries for 6
// scaled minutes, so a FLUSH waits it out and the lease holds.
func TestOutageThroughFaultProxy(t *testing.T) {
	options, b2 := realOptions(t)
	if b2 {
		t.Skip("the fault proxy needs the local MinIO")
	}
	b := realBucket(t, options, false, 200*time.Millisecond)
	proxy, err := s3fault.New(t.Context(), options.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	options.Endpoint = proxy.URL()
	through, err := newBucket(options, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	through.prefix, through.timeout = b.prefix, 6*time.Second
	tune := realTuning()
	tune.renewEvery = time.Second
	tune.lease = 8 * time.Second
	tune.stale = 10 * time.Second
	e := openReal(t, through, t.TempDir(), tune)
	writeFile(t, e, "before", "written before the outage")

	e.timesMu.Lock()
	renewed := e.leaseUntil
	e.timesMu.Unlock()
	waitFor(t, "a renewal", func() bool {
		e.timesMu.Lock()
		defer e.timesMu.Unlock()
		return e.leaseUntil.After(renewed)
	})
	proxy.FailS3For(5 * time.Second)
	start := time.Now()
	writeFile(t, e, "during", "written during the outage")
	if waited := time.Since(start); waited < 4*time.Second {
		t.Fatalf("the FLUSH returned after %s, inside the outage", waited)
	}
	time.Sleep(2 * time.Second)
	if err = e.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"before": "written before the outage", "during": "written during the outage"}
	if got := tree(t, e); !maps.Equal(got, want) {
		t.Fatalf("tree = %q", got)
	}
	shutdown(t, e)
}
