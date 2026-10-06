// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"bytes"
	"errors"
	"strings"
)

// Bytes sums the sizes of the objects whose keys are in set. Keys the bucket
// no longer has count as zero.
func Bytes(objects map[string]int64, set map[string]bool) int64 {
	var total int64
	for key := range set {
		total += objects[key]
	}
	return total
}

// B2Region reads the region from a B2 endpoint such as
// https://s3.eu-central-003.backblazeb2.com.
func B2Region(endpoint string) (string, error) {
	host, ok := strings.CutPrefix(endpoint, "https://")
	parts := strings.Split(host, ".")
	if !ok || len(parts) != 4 || parts[0] != "s3" || parts[1] == "" || parts[2] != "backblazeb2" || parts[3] != "com" {
		return "", errors.New("B2_ENDPOINT must look like https://s3.<region>.backblazeb2.com")
	}
	return parts[1], nil
}

// Marker starts every 4 KiB block of the thinning scenario's file, so its
// data can be found in the chunk objects.
var Marker = []byte("s3-smb thinning block marker 620")

// HoldsMarker reports whether data holds a block of the thinning file.
func HoldsMarker(data []byte) bool {
	return bytes.Contains(data, Marker)
}
