// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"howett.net/plist"
)

func attachment(t *testing.T, device, mount string) string {
	t.Helper()
	data, err := plist.Marshal(map[string]any{"system-entities": []map[string]string{
		{"dev-entry": device}, {"dev-entry": device + "s2", "mount-point": mount},
	}}, plist.XMLFormat)
	must(t, err)
	return string(data)
}

func TestMacFeatures(t *testing.T) {
	const finderInfo = "544558547333736D0000000000000000\n00000000000000000000000000000000\n"
	for _, tc := range []struct {
		name, user, finder, attached string
		wantError                    bool
		wantCalls                    int
	}{
		{name: "round-trip", user: "user attribute round-trip\n", finder: finderInfo, attached: attachment(t, "/dev/disk42", "/Volumes/s3-smb-m4"), wantCalls: 8},
		{name: "user mismatch", user: "changed\n", wantError: true, wantCalls: 2},
		{name: "FinderInfo malformed", user: "user attribute round-trip\n", finder: "ZZ", wantError: true, wantCalls: 4},
		{name: "FinderInfo short", user: "user attribute round-trip\n", finder: "54455854", wantError: true, wantCalls: 4},
		{name: "FinderInfo changed", user: "user attribute round-trip\n", finder: strings.Repeat("00", 32), wantError: true, wantCalls: 4},
		{name: "attachment has no device", user: "user attribute round-trip\n", finder: finderInfo, attached: `<plist version="1.0"><dict><key>system-entities</key><array/></dict></plist>`, wantError: true, wantCalls: 7},
		{name: "attachment malformed", user: "user attribute round-trip\n", finder: finderInfo, attached: "not a plist", wantError: true, wantCalls: 7},
		{name: "attachment not mounted", user: "user attribute round-trip\n", finder: finderInfo, attached: attachment(t, "/dev/disk42", ""), wantError: true, wantCalls: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			share := t.TempDir()
			calls := 0
			command := func(args ...string) (string, error) {
				calls++
				switch calls - 1 {
				case 1:
					return tc.user, nil
				case 3:
					return tc.finder, nil
				case 6:
					return tc.attached, nil
				default:
					return "", nil
				}
			}
			err := MacFeatures(command, share, "/task/bin/fullsync")
			if (err != nil) != tc.wantError || calls != tc.wantCalls {
				t.Fatalf("error = %v, calls = %d; want error %v, calls %d", err, calls, tc.wantError, tc.wantCalls)
			}
			entries, err := os.ReadDir(share)
			must(t, err)
			if (!tc.wantError && len(entries) != 0) || (tc.wantError && len(entries) != 1) {
				t.Fatal("wrong fixture cleanup", entries)
			}
		})
	}
}

func TestMacFeatureCommands(t *testing.T) {
	injected := errors.New("native command failed")
	// Every native failure must fail acceptance, including F_FULLFSYNC and detach.
	for failAt := -1; failAt < 8; failAt++ {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			share := t.TempDir()
			var expected [][]string
			calls := 0
			command := func(args ...string) (string, error) {
				if calls == 0 {
					expected = featureCommands(t, share, args[len(args)-1])
				}
				if calls >= len(expected) || !slices.Equal(args, expected[calls]) {
					t.Fatal("unexpected command", calls, args)
				}
				index := calls
				calls++
				if index == failAt {
					return "", injected
				}
				switch index {
				case 1:
					return "user attribute round-trip\n", nil
				case 3:
					return expected[2][3], nil
				case 6:
					return attachment(t, "/dev/disk42", "/Volumes/s3-smb-m4"), nil
				default:
					return "", nil
				}
			}
			err := MacFeatures(command, share, "/task/bin/fullsync")
			if failAt == -1 {
				must(t, err)
				if calls != 8 {
					t.Fatal("missing commands", calls)
				}
			} else if !errors.Is(err, injected) || calls != failAt+1 {
				t.Fatal("native failure was not preserved", calls, err)
			}
		})
	}
}

func featureCommands(t *testing.T, share, path string) [][]string {
	t.Helper()
	directory := filepath.Dir(path)
	if filepath.Dir(directory) != share {
		t.Fatal("fixture is not on the share", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // This is the fixture just created under the test-owned temporary share.
	must(t, err)
	if string(data) != "Mac stream acceptance\n" {
		t.Fatal("fixture contents", string(data))
	}
	bundle := filepath.Join(directory, "interop.sparsebundle")
	return [][]string{
		{"/usr/bin/xattr", "-w", "user.s3-smb-acceptance", "user attribute round-trip", path},
		{"/usr/bin/xattr", "-p", "user.s3-smb-acceptance", path},
		{"/usr/bin/xattr", "-wx", "com.apple.FinderInfo", "544558547333736d000000000000000000000000000000000000000000000000", path},
		{"/usr/bin/xattr", "-px", "com.apple.FinderInfo", path},
		{"/task/bin/fullsync", filepath.Join(directory, "full-sync")},
		{"/usr/bin/hdiutil", "create", "-type", "SPARSEBUNDLE", "-size", "64m", "-fs", "HFS+", "-volname", "s3-smb-m4", bundle},
		{"/usr/bin/hdiutil", "attach", "-nobrowse", "-plist", bundle},
		{"/usr/bin/hdiutil", "detach", "/dev/disk42"},
	}
}

func TestMacFeaturesNeedsShare(t *testing.T) {
	called := false
	err := MacFeatures(func(...string) (string, error) {
		called = true
		return "", nil
	}, filepath.Join(t.TempDir(), "missing"), "/task/bin/fullsync")
	if err == nil || called {
		t.Fatal("missing share must fail before commands", called, err)
	}
}
