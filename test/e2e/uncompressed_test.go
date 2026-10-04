// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestNewVolumeStoresUncompressedObjects(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	identity, err := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/format.json")})
	if err != nil {
		t.Fatal(err)
	}
	var format struct{ Compression string }
	decodeErr := json.NewDecoder(identity.Body).Decode(&format)
	closeErr := identity.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if format.Compression != "none" {
		t.Fatalf("new volume compression = %q, want none", format.Compression)
	}

	data := bytes.Repeat([]byte("uncompressed file data\n"), 1024)
	share, closeShare := f.share()
	writeFile(t, share, "data.bin", data)
	verifyFiles(t, share, map[string][]byte{"data.bin": data})
	closeShare()
	d.stop()

	objects, err := f.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket), Prefix: aws.String("s3-smb/chunks/")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToBool(objects.IsTruncated) || len(objects.Contents) != 1 {
		t.Fatalf("expected one data object, got %d (truncated: %t)", len(objects.Contents), aws.ToBool(objects.IsTruncated))
	}
	obj, err := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: objects.Contents[0].Key})
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(obj.Body)
	closeErr = obj.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !bytes.Equal(stored, data) {
		t.Fatal("stored object differs from the uncompressed file bytes")
	}
}
