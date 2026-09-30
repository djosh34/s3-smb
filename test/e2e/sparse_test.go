// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smb "github.com/hirochachacha/go-smb2"
)

// Exercise signed SMB EOF/WRITE/FLUSH against encrypted, uncompressed storage.
// This is not an Apple sparsebundle-formatting or FSCTL_SET_SPARSE test.
func TestSMBSparseEOFAndOverwriteObjectGrowth(t *testing.T) {
	f := newFixture(t, true) // Omitted compression: the released rc6 none path.
	proxy := newFaultProxy(t, f.endpoint)
	f.endpoint = proxy.URL() // f.store still measures the real MinIO directly.
	d := f.start()
	share, closeShare := f.share()
	// Explicit success teardown precedes daemon stop; deferred failure cleanup
	// must not issue a second SMB disconnect after the connection is closed.
	closeShare = sync.OnceFunc(closeShare)
	defer closeShare()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	share = share.WithContext(ctx)

	identity, err := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/format.json")})
	if err != nil {
		t.Fatal(err)
	}
	var format struct {
		Compression, EncryptAlgo string
		BlockSize                int
	}
	err = json.NewDecoder(identity.Body).Decode(&format)
	identity.Body.Close()
	if err != nil || format.Compression != "none" || format.EncryptAlgo == "" || format.BlockSize <= 0 {
		t.Fatalf("expected native encrypted none format: %+v, %v", format, err)
	}
	capacity, err := share.Statfs(".")
	if err != nil {
		t.Fatal(err)
	}
	unit := capacity.BlockSize() * capacity.FragmentSize()
	t.Logf("wire FS capacity: allocation_unit_bytes=%d total_bytes=%d available_bytes=%d (logical advertised capacity, NOT backend free space)",
		unit, capacity.TotalBlockCount()*unit, capacity.AvailableBlockCount()*unit)

	file, err := share.OpenFile("large-sparse.bin", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	closeFile := sync.OnceValue(file.Close)
	defer closeFile()
	type observation struct{ chunkBytes, chunks, allBytes, puts int64 }
	observe := func(stage string) observation {
		t.Helper()
		var result observation
		pages := s3.NewListObjectsV2Paginator(f.store, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
		for pages.HasMorePages() {
			page, e := pages.NextPage(ctx)
			if e != nil {
				t.Fatal(e)
			}
			for _, object := range page.Contents {
				size := aws.ToInt64(object.Size)
				result.allBytes += size
				if strings.Contains(aws.ToString(object.Key), "/chunks/") {
					result.chunkBytes += size
					result.chunks++
				}
			}
		}
		result.puts = proxy.chunkPuts.Load()
		info, e := file.Stat()
		if e != nil {
			t.Fatal(e)
		}
		stat, ok := info.(*smb.FileStat)
		if !ok {
			t.Fatalf("unexpected SMB stat type %T", info)
		}
		t.Logf("%s: EOF=%d SMB_AllocationSize=%d S3_chunk_objects=%d S3_chunk_payload_bytes=%d S3_all_payload_bytes=%d successful_chunk_PUTs=%d",
			stage, stat.EndOfFile, stat.AllocationSize, result.chunks, result.chunkBytes, result.allBytes, result.puts)
		return result
	}
	read := func(offset int64, want []byte) {
		t.Helper()
		got := make([]byte, len(want))
		if n, e := file.ReadAt(got, offset); e != nil || n != len(got) || !bytes.Equal(got, want) {
			t.Fatalf("SMB read at %d: bytes=%d error=%v match=%t", offset, n, e, bytes.Equal(got, want))
		}
	}
	flush := func() {
		t.Helper()
		if e := file.Sync(); e != nil {
			t.Fatal(e)
		}
	}
	before := observe("empty")
	const logicalSize int64 = 256 << 20
	if err = file.Truncate(logicalSize); err != nil {
		t.Fatal(err)
	}
	flush()
	grown := observe("256MiB-logical-EOF-no-writes")
	if grown.chunkBytes != before.chunkBytes || grown.puts != before.puts {
		t.Fatal("SMB logical growth without writes materialized data objects")
	}
	if info, e := file.Stat(); e != nil || info.Size() != logicalSize {
		t.Fatalf("logical EOF after sparse grow: %v %v", info, e)
	}
	read(0, make([]byte, 64))
	read(logicalSize/2, make([]byte, 64))
	read(logicalSize-64, make([]byte, 64))

	// Cross a 4KiB boundary, but remain within one native data block.
	const offset = logicalSize - (1 << 20) + 4093
	payload := bytes.Repeat([]byte("sparse-data-"), 6)
	write := func(data []byte) {
		t.Helper()
		if n, e := file.WriteAt(data, offset); e != nil || n != len(data) {
			t.Fatalf("tiny high-offset SMB write: %d %v", n, e)
		}
		flush()
	}
	write(payload)
	written := observe("tiny-high-offset-write-and-flush")
	// A single-block write may allocate a whole native block plus encryption
	// framing, but must not materialize the unwritten logical extent.
	blockEnvelope := int64(format.BlockSize)*1024 + 4096
	if written.puts <= grown.puts || written.chunkBytes <= grown.chunkBytes || written.chunkBytes-grown.chunkBytes > blockEnvelope {
		t.Fatalf("single-block write growth: before=%+v after=%+v native_block_plus_framing_bound=%d", grown, written, blockEnvelope)
	}
	read(offset-32, append(append(make([]byte, 32), payload...), make([]byte, 32)...))
	read(0, make([]byte, 64))
	read(logicalSize-64, make([]byte, 64))

	const overwrites = 8
	for i := 0; i < overwrites; i++ {
		payload[0] = byte('A' + i)
		write(payload)
		read(offset, payload)
	}
	after := observe("eight-same-range-overwrites-each-flushed")
	if delta := after.puts - written.puts; delta <= 0 || after.chunkBytes-written.chunkBytes > delta*blockEnvelope {
		t.Fatalf("overwrite growth exceeds observed PUTs times native block/framing: before=%+v after=%+v", written, after)
	}
	t.Logf("overwrite measurement: submitted_bytes=%d added_chunk_payload_bytes=%d additional_PUTs=%d; retained COW objects are not unwritten-hole allocation or an Apple amplification ratio",
		overwrites*len(payload), after.chunkBytes-written.chunkBytes, after.puts-written.puts)
	read(offset-32, append(append(make([]byte, 32), payload...), make([]byte, 32)...))
	if err = closeFile(); err != nil {
		t.Fatal(err)
	}
	closeShare()
	d.stop()
}
