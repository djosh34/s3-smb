// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"howett.net/plist"
)

// MacFeatures checks native Mac operations on the mounted share. Failed fixtures
// stay on the share for diagnosis; the harness detaches images during cleanup.
func MacFeatures(command func(...string) (string, error), share, fullSync string) error {
	directory, err := os.MkdirTemp(share, "m4-features-")
	if err != nil {
		return err
	}
	path := filepath.Join(directory, "attributes")
	if err := os.WriteFile(path, []byte("Mac stream acceptance\n"), 0o600); err != nil {
		return err
	}
	if err := checkXattrs(command, path); err != nil {
		return err
	}
	if _, err := command(fullSync, filepath.Join(directory, "full-sync")); err != nil {
		return fmt.Errorf("F_FULLFSYNC: %w", err)
	}
	bundle := filepath.Join(directory, "interop.sparsebundle")
	if _, err := command("/usr/bin/hdiutil", "create", "-type", "SPARSEBUNDLE", "-size", "64m", "-fs", "HFS+", "-volname", "s3-smb-m4", bundle); err != nil {
		return err
	}
	if err := sparsebundleRoundTrip(command, bundle); err != nil {
		return err
	}
	return os.RemoveAll(directory)
}

func checkXattrs(command func(...string) (string, error), path string) error {
	const attribute, value = "user.s3-smb-acceptance", "user attribute round-trip"
	if _, err := command("/usr/bin/xattr", "-w", attribute, value, path); err != nil {
		return err
	}
	output, err := command("/usr/bin/xattr", "-p", attribute, path)
	if err != nil {
		return err
	}
	if strings.TrimSuffix(output, "\n") != value {
		return fmt.Errorf("user xattr mismatch: %q", output)
	}
	// A file's FinderInfo is exactly 32 bytes. Use a valid type and creator,
	// followed by zero flags, location and reserved fields.
	const finderInfo = "544558547333736d000000000000000000000000000000000000000000000000"
	if _, err = command("/usr/bin/xattr", "-wx", "com.apple.FinderInfo", finderInfo, path); err != nil {
		return err
	}
	output, err = command("/usr/bin/xattr", "-px", "com.apple.FinderInfo", path)
	if err != nil {
		return err
	}
	actual, err := hex.DecodeString(strings.Join(strings.Fields(output), ""))
	if err != nil {
		return fmt.Errorf("FinderInfo hex: %w", err)
	}
	if len(actual) != 32 || hex.EncodeToString(actual) != finderInfo {
		return fmt.Errorf("FinderInfo mismatch: %x", actual)
	}
	return nil
}

func sparsebundleRoundTrip(command func(...string) (string, error), bundle string) (err error) {
	output, err := command("/usr/bin/hdiutil", "attach", "-nobrowse", "-plist", bundle)
	if err != nil {
		return err
	}
	var attached struct {
		Entities []struct {
			Device string `plist:"dev-entry"`
			Mount  string `plist:"mount-point"`
		} `plist:"system-entities"`
	}
	if _, err = plist.Unmarshal([]byte(output), &attached); err != nil {
		return err
	}
	device, mounts := "", 0
	for _, entity := range attached.Entities {
		if device == "" && entity.Device != "" {
			device = entity.Device
		}
		if entity.Mount != "" {
			mounts++
		}
	}
	if device == "" {
		return errors.New("sparsebundle attachment has no device")
	}
	defer func() {
		_, detachErr := command("/usr/bin/hdiutil", "detach", device)
		err = errors.Join(err, detachErr)
	}()
	if mounts != 1 {
		return fmt.Errorf("sparsebundle attachment has %d mounted volumes, want one", mounts)
	}
	return nil
}
