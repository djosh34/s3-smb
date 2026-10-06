// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smb "github.com/hirochachacha/go-smb2"
)

func TestFilesystemOperations(t *testing.T) {
	f := newFixture(t)
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

// TestMissingDataIsSMBError deletes the chunks of a file from the bucket.
// Reading the file on a new data folder must fail with an SMB error, not
// return an empty or short file.
func TestMissingDataIsSMBError(t *testing.T) {
	f := newFixture(t)
	d := f.start()
	s, disconnect := f.share()
	writeFile(t, s, "missing.bin", bytes.Repeat([]byte("missing data must not be empty-file success\n"), 10000))
	disconnect()
	f.copyDatabase(d)
	chunks := f.keys("chunks/")
	if len(chunks) == 0 {
		t.Fatal("the file has no chunks in the bucket")
	}
	for _, key := range chunks {
		if _, err := f.store.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)}); err != nil {
			t.Fatal(err)
		}
	}
	f.freshLocal()
	d = f.start()
	s, disconnect = f.share()
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Minute)
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
	disconnect()
	d.stop()
}
