// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smb "github.com/hirochachacha/go-smb2"
)

func TestFilesystemOperations(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	s, disconnect := f.share()
	if err := s.Mkdir("directory", 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s, "directory/original", []byte("original data"))
	if err := s.Rename("directory/original", "directory/renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat("directory/original"); err == nil {
		t.Fatal("rename left original path")
	}
	stamp := time.Unix(1700000000, 0)
	if err := s.Chtimes("directory/renamed", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat("directory/renamed")
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatalf("mtime got %s want %s", info.ModTime(), stamp)
	}
	if err = s.Truncate("directory/renamed", 8); err != nil {
		t.Fatal(err)
	}
	verifyFiles(t, s, map[string][]byte{"directory/renamed": []byte("original")})
	entries, err := s.ReadDir("directory")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "renamed" {
		t.Fatalf("unexpected directory entries %v", entries)
	}
	if err = s.Remove("directory/renamed"); err != nil {
		t.Fatal(err)
	}
	if err = s.Remove("directory"); err != nil {
		t.Fatal(err)
	}
	disconnect()
	d.stop()
}

func TestZeroCacheUnusableDirectory(t *testing.T) {
	f := newFixture(t, false)
	// A regular file cannot contain cache blocks. Explicit zero must neither use
	// this path nor silently choose a positive fallback directory.
	stale := []byte("old cache path must be ignored")
	if err := os.WriteFile(filepath.Join(f.root, "cache"), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	d := f.start()
	s, disconnect := f.share()
	data := bytes.Repeat([]byte("zero cache MinIO data\n"), 100000)
	writeFile(t, s, "remote.bin", data)
	disconnect()
	d.stop()
	d = f.start()
	s, disconnect = f.share()
	verifyFiles(t, s, map[string][]byte{"remote.bin": data})
	disconnect()
	d.stop()
	got, err := os.ReadFile(filepath.Join(f.root, "cache"))
	if err != nil || !bytes.Equal(got, stale) {
		t.Fatalf("zero-cache path was touched: %v", err)
	}
}

func TestResourceForkOffsetsAndResize(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	s, disconnect := f.share()
	base := []byte("ordinary file content stays unchanged")
	writeFile(t, s, "forked.bin", base)
	stream, err := s.OpenFile("forked.bin:AFP_Resource", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := stream.WriteAt([]byte("abcdef"), 0); err != nil || n != 6 {
		t.Fatalf("initial stream write: %d %v", n, err)
	}
	if n, err := stream.WriteAt([]byte("XY"), 2); err != nil || n != 2 {
		t.Fatalf("offset stream write: %d %v", n, err)
	}
	check := func(want []byte) {
		t.Helper()
		got, err := s.ReadFile("forked.bin:AFP_Resource")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("resource fork: got %q want %q error %v", got, want, err)
		}
	}
	check([]byte("abXYef"))
	if err := stream.Truncate(3); err != nil {
		t.Fatal(err)
	}
	check([]byte("abX"))
	if err := stream.Truncate(6); err != nil {
		t.Fatal(err)
	}
	want := []byte{'a', 'b', 'X', 0, 0, 0}
	check(want)
	if n, err := stream.WriteAt([]byte("Z"), 5); err != nil || n != 1 {
		t.Fatalf("extended offset write: %d %v", n, err)
	}
	want[5] = 'Z'
	check(want)
	// The pinned native xattr capacity is 64 KiB, not arbitrary stream size.
	for _, operation := range []func() error{func() error { _, err := stream.WriteAt([]byte("!"), 65536); return err }, func() error { return stream.Truncate(65537) }} {
		err := operation()
		var response *smb.ResponseError
		if !errors.As(err, &response) {
			t.Fatalf("over-limit resource fork operation must return SMB error: %v", err)
		}
		check(want)
	}
	if err := stream.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	verifyFiles(t, s, map[string][]byte{"forked.bin": base, "forked.bin:AFP_Resource": want})
	disconnect()
	f.protectedAfter(time.Now())
	d.stop()
	f.freshLocal()
	d = f.start()
	s, disconnect = f.share()
	verifyFiles(t, s, map[string][]byte{"forked.bin": base, "forked.bin:AFP_Resource": want})
	disconnect()
	d.stop()
}

func TestMissingDataIsSMBError(t *testing.T) {
	if os.Getenv("S3_SMB_CHECK_MODE") != "gate" {
		t.Skip("permanent missing reads exhaust at least 361.829s of retries; run gate mode")
	}
	f := newFixture(t, false)
	d := f.start()
	s, disconnect := f.share()
	writeFile(t, s, "missing.bin", bytes.Repeat([]byte("missing data must not be empty-file success\n"), 10000))
	disconnect()
	f.protectedAfter(time.Now())
	d.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	objects, err := f.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	deleted := 0
	for _, obj := range objects.Contents {
		if strings.Contains(aws.ToString(obj.Key), "/chunks/") {
			if _, deleteErr := f.store.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: obj.Key}); deleteErr != nil {
				t.Fatal(deleteErr)
			}
			deleted++
		}
	}
	if deleted == 0 {
		t.Fatal("fixture did not create any data objects")
	}
	f.freshLocal()
	d = f.start()
	s, disconnect = f.share()
	readCtx, readCancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer readCancel()
	s = s.WithContext(readCtx)
	got, err := s.ReadFile("missing.bin")
	if err == nil {
		t.Fatalf("missing referenced data returned successful %d-byte file", len(got))
	}
	var response *smb.ResponseError
	if !errors.As(err, &response) {
		t.Fatalf("expected explicit SMB error rather than connection/timeout failure: %T %v", err, err)
	}
	t.Logf("deleted referenced chunks=%d explicit SMB error=%v", deleted, response)
	disconnect()
	d.stop()
}
