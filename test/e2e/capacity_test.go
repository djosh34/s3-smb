// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import "testing"

// TestStorageCapacity checks that the share reports the configured capacity,
// and that a restart with another value reports the new one.
func TestStorageCapacity(t *testing.T) {
	f := newFixture(t)
	for _, capacity := range []struct {
		setting string
		bytes   uint64
	}{{"100 MB", 100_000_000}, {"200 MB", 200_000_000}} {
		f.storageCapacity = capacity.setting
		d := f.start()
		share, disconnect := f.share()
		info, err := share.Statfs(".")
		if err != nil {
			t.Fatal(err)
		}
		// The share reports whole allocation units.
		unit := info.BlockSize() * info.FragmentSize()
		if got, want := info.TotalBlockCount()*unit, capacity.bytes/unit*unit; got != want {
			t.Fatalf("capacity %s: the share reports %d bytes, want %d", capacity.setting, got, want)
		}
		disconnect()
		d.stop()
	}
}
