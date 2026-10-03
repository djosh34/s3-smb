// SPDX-License-Identifier: AGPL-3.0-only
package chunk

import (
	"bytes"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/compress"
)

// A Page.Slice must be safe as a decoder destination while neighboring bytes
// belong to other slices of the same file-read page.
func TestPagePoolZstdSliceBoundary(t *testing.T) {
	const offset, size = 37, 521
	codec := compress.NewCompressor("zstd")
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i*17 + 18)
	}
	compressed := make([]byte, codec.CompressBound(size))
	n, err := codec.Compress(compressed, src)
	if err != nil {
		t.Fatal(err)
	}
	root := NewOffPage(4096)
	defer root.Release()
	for i := range root.Data {
		root.Data[i] = 0x73
	}
	dst := root.Slice(offset, size)
	defer dst.Release()
	n, err = codec.Decompress(dst.Data, compressed[:n])
	if err != nil || n != size || !bytes.Equal(dst.Data, src) {
		t.Fatalf("decompress: n=%d err=%v", n, err)
	}
	first, last, count := -1, -1, 0
	for i, b := range root.Data {
		if (i < offset || i >= offset+size) && b != 0x73 {
			if first < 0 {
				first = i
			}
			last = i
			count++
		}
	}
	if count > 0 {
		t.Fatalf("decoder changed neighboring page slice: first=%d last=%d count=%d", first, last, count)
	}
}
