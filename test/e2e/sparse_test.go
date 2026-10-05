// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smb "github.com/hirochachacha/go-smb2"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

// TestSMBSparseEOFAndOverwriteObjectGrowth checks that growing a file with SET
// EOF stores nothing in S3, and that small writes and overwrites at a high
// offset store about one block each. It does not use FSCTL_SET_SPARSE.
func TestSMBSparseEOFAndOverwriteObjectGrowth(t *testing.T) {
	f := newFixture(t, true)
	proxy := f.newFaultProxy()
	f.start()
	share, disconnect := f.share()
	t.Cleanup(disconnect)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	share = share.WithContext(ctx)
	// A write may store a whole block plus encryption framing.
	blockEnvelope := blockSize(t, f)*1024 + 4096

	file, err := share.OpenFile("large-sparse.bin", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	before := chunkUsage(t, f, proxy)
	const logicalSize int64 = 256 << 20
	if err = file.Truncate(logicalSize); err != nil {
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
	grown := chunkUsage(t, f, proxy)
	if grown != before {
		t.Fatalf("growing the file stored data: before %+v, after %+v", before, grown)
	}
	if info, err := file.Stat(); err != nil || info.Size() != logicalSize {
		t.Fatalf("size after growing: %v %v", info, err)
	}
	zeros := make([]byte, 64)
	readAt(t, file, 0, zeros)
	readAt(t, file, logicalSize/2, zeros)
	readAt(t, file, logicalSize-64, zeros)

	// Cross a 4 KiB boundary, but stay within one block.
	const offset = logicalSize - (1 << 20) + 4093
	payload := bytes.Repeat([]byte("sparse-data-"), 6)
	writeAt(t, file, offset, payload)
	written := chunkUsage(t, f, proxy)
	if written.puts <= grown.puts || written.chunkBytes <= grown.chunkBytes || written.chunkBytes-grown.chunkBytes > blockEnvelope {
		t.Fatalf("one small write: before %+v, after %+v, bound %d", grown, written, blockEnvelope)
	}
	readAt(t, file, offset-32, append(append(make([]byte, 32), payload...), make([]byte, 32)...))
	readAt(t, file, 0, zeros)
	readAt(t, file, logicalSize-64, zeros)

	for i := range 8 {
		payload[0] = byte('A' + i)
		writeAt(t, file, offset, payload)
		readAt(t, file, offset, payload)
	}
	after := chunkUsage(t, f, proxy)
	if delta := after.puts - written.puts; delta <= 0 || after.chunkBytes-written.chunkBytes > delta*blockEnvelope {
		t.Fatalf("overwrites stored more than one block per PUT: before %+v, after %+v", written, after)
	}
	readAt(t, file, offset-32, append(append(make([]byte, 32), payload...), make([]byte, 32)...))
}

// blockSize returns the volume block size in KiB.
func blockSize(t *testing.T, f *fixture) int64 {
	t.Helper()
	identity, err := f.store.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/format.json")})
	if err != nil {
		t.Fatal(err)
	}
	var format struct{ BlockSize int64 }
	if err = errors.Join(json.NewDecoder(identity.Body).Decode(&format), identity.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if format.BlockSize <= 0 {
		t.Fatalf("block size %d", format.BlockSize)
	}
	return format.BlockSize
}

type usage struct{ chunkBytes, puts int64 }

// chunkUsage returns the bytes in chunk objects and the chunk PUTs so far.
func chunkUsage(t *testing.T, f *fixture, proxy *s3fault.Proxy) usage {
	t.Helper()
	var result usage
	pages := s3.NewListObjectsV2Paginator(f.store, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Contents {
			if strings.Contains(aws.ToString(object.Key), "/chunks/") {
				result.chunkBytes += aws.ToInt64(object.Size)
			}
		}
	}
	result.puts = proxy.ChunkPuts()
	return result
}

func readAt(t *testing.T, file *smb.File, offset int64, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	if n, err := file.ReadAt(got, offset); err != nil || n != len(got) || !bytes.Equal(got, want) {
		t.Fatalf("read at %d: %d bytes, error %v, match %t", offset, n, err, bytes.Equal(got, want))
	}
}

// writeAt writes and flushes data at offset.
func writeAt(t *testing.T, file *smb.File, offset int64, data []byte) {
	t.Helper()
	if n, err := file.WriteAt(data, offset); err != nil || n != len(data) {
		t.Fatalf("write at %d: %d bytes, error %v", offset, n, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
}
