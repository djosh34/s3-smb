// SPDX-License-Identifier: AGPL-3.0-only
package compress

import (
	"bytes"
	"testing"
)

// A caller can give the codec a window of a larger read buffer. Bytes outside
// that window belong to other reads, even when its capacity includes them.
// This test deliberately uses ordinary Go memory, not Page or an FS fixture.
func TestDecompressDestinationBoundary(t *testing.T) {
	const offset, size = 37, 521
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i*17 + 18)
	}
	for _, name := range []string{"none", "lz4", "zstd"} {
		t.Run(name, func(t *testing.T) {
			codec := NewCompressor(name)
			encoded := make([]byte, codec.CompressBound(size))
			n, err := codec.Compress(encoded, src)
			if err != nil {
				t.Fatal(err)
			}
			for _, clamp := range []bool{false, true} {
				label := "spare-capacity"
				if clamp {
					label = "clamped-capacity"
				}
				t.Run(label, func(t *testing.T) {
					root := bytes.Repeat([]byte{0x73}, 4096)
					dst := root[offset : offset+size]
					if clamp {
						dst = dst[:len(dst):len(dst)]
					}
					got, err := codec.Decompress(dst, encoded[:n])
					if err != nil || got != size || !bytes.Equal(dst, src) {
						t.Fatalf("decode: n=%d err=%v outputMatches=%v", got, err, bytes.Equal(dst, src))
					}
					first, last, changed := -1, -1, 0
					for i, b := range root {
						if (i < offset || i >= offset+size) && b != 0x73 {
							if first == -1 {
								first = i
							}
							last, changed = i, changed+1
						}
					}
					if changed != 0 {
						t.Fatalf("neighbor overwrite: dst len=%d cap=%d first=%d last=%d changed=%d", len(dst), cap(dst), first, last, changed)
					}
				})
			}
		})
	}
}
