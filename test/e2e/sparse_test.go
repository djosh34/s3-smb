// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smb "github.com/hirochachacha/go-smb2"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

// TestSMBSparseEOFAndOverwriteObjectGrowth checks that growing a file with SET
// EOF stores nothing in S3, and that each FLUSH after a small write or
// overwrite at a high offset uploads only the one chunk it touched. It does
// not use FSCTL_SET_SPARSE.
func TestSMBSparseEOFAndOverwriteObjectGrowth(t *testing.T) {
	f := newFixture(t)
	proxy := f.newFaultProxy()
	f.start()
	share, disconnect := f.share()
	t.Cleanup(disconnect)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	share = share.WithContext(ctx)

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

	// Cross a 4 KiB boundary, but stay within one chunk.
	const offset = logicalSize - (1 << 20) + 4093
	payload := bytes.Repeat([]byte("sparse-data-"), 6)
	writeAt(t, file, offset, payload)
	written := chunkUsage(t, f, proxy)
	if written.puts != grown.puts+1 || written.chunkBytes <= grown.chunkBytes || written.chunkBytes-grown.chunkBytes > chunkSize {
		t.Fatalf("one small write: before %+v, after %+v, bound one chunk of %d bytes", grown, written, chunkSize)
	}
	readAt(t, file, offset-32, append(append(make([]byte, 32), payload...), make([]byte, 32)...))
	readAt(t, file, 0, zeros)
	readAt(t, file, logicalSize-64, zeros)

	const overwrites = 8
	for i := range overwrites {
		payload[0] = byte('A' + i)
		writeAt(t, file, offset, payload)
		readAt(t, file, offset, payload)
	}
	// Replaced chunks stay in the bucket until no kept copy needs them.
	after := chunkUsage(t, f, proxy)
	if after.puts != written.puts+overwrites || after.chunkBytes-written.chunkBytes > overwrites*chunkSize {
		t.Fatalf("each overwrite must upload one chunk: before %+v, after %+v", written, after)
	}
	readAt(t, file, offset-32, append(append(make([]byte, 32), payload...), make([]byte, 32)...))
}

type usage struct{ chunkBytes, puts int64 }

// chunkUsage returns the bytes in chunk objects and the chunk PUTs so far.
func chunkUsage(t *testing.T, f *fixture, proxy *s3fault.Proxy) usage {
	t.Helper()
	var result usage
	pages := s3.NewListObjectsV2Paginator(f.store, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket), Prefix: aws.String("chunks/")})
	for pages.HasMorePages() {
		page, err := pages.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Contents {
			result.chunkBytes += aws.ToInt64(object.Size)
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
