//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// shareFeatures checks Mac file operations on the mounted share: a user xattr,
// FinderInfo, F_FULLFSYNC and an HFS+ sparsebundle that attaches and detaches.
// A failure leaves the files on the share for diagnosis; finish detaches the
// image.
func (h *harness) shareFeatures() {
	directory, err := os.MkdirTemp(h.share, "features-")
	h.must(err)
	path := filepath.Join(directory, "attributes")
	h.must(os.WriteFile(path, []byte("Mac stream acceptance\n"), 0o600))
	const attribute, value = "user.s3-smb-acceptance", "user attribute round-trip"
	h.run(5*time.Minute, "/usr/bin/xattr", "-w", attribute, value, path)
	if output := h.run(5*time.Minute, "/usr/bin/xattr", "-p", attribute, path); strings.TrimSuffix(output, "\n") != value {
		h.t.Fatalf("user xattr mismatch: %q", output)
	}
	// A file's FinderInfo is exactly 32 bytes: a type and creator, then zero
	// flags, location and reserved fields.
	const finderInfo = "544558547333736d000000000000000000000000000000000000000000000000"
	h.run(5*time.Minute, "/usr/bin/xattr", "-wx", "com.apple.FinderInfo", finderInfo, path)
	output := h.run(5*time.Minute, "/usr/bin/xattr", "-px", "com.apple.FinderInfo", path)
	// xattr prints the hex in groups over several lines.
	actual, err := hex.DecodeString(strings.Join(strings.Fields(output), ""))
	h.must(err)
	if hex.EncodeToString(actual) != finderInfo {
		h.t.Fatalf("FinderInfo mismatch: %x", actual)
	}
	h.run(5*time.Minute, filepath.Join(h.bin, "fullsync"), directory)
	bundle := filepath.Join(directory, "interop.sparsebundle")
	h.run(5*time.Minute, "/usr/bin/hdiutil", "create", "-type", "SPARSEBUNDLE", "-size", "64m", "-fs", "HFS+", "-volname", "s3-smb-features", bundle)
	devices, volumes := h.attach(bundle, false)
	if len(devices) == 0 || len(volumes) != 1 {
		h.t.Fatalf("sparsebundle attached as devices %v with volumes %v, want one volume", devices, volumes)
	}
	h.must(h.detachDevice(devices[0]))
	h.must(os.RemoveAll(directory))
}
