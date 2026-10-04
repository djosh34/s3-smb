// SPDX-License-Identifier: AGPL-3.0-only
package storage

import "testing"

func TestValidateFormatCompression(t *testing.T) {
	for _, codec := range []string{"none", "zstd", "lz4"} {
		t.Run(codec, func(t *testing.T) {
			format, err := NewFormat(VolumeName, false, 14)
			if err != nil {
				t.Fatal(err)
			}
			format.Compression = codec
			err = validateFormat(format)
			if codec == "none" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != "unsupported compression" {
				t.Fatalf("compression %q: expected unsupported compression, got %v", codec, err)
			}
		})
	}
}
