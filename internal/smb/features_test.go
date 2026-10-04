package smb_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestFeatureMasks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"negotiate", uint64(smb.AdvertisedCapabilities), 0x04},
		{"filesystem", uint64(smb.AdvertisedFilesystemAttributes), 0x07},
		{"aapl-not-enabled", smb.AAPLVolumeCapabilities, 0},
		{"aapl-case-sensitive", smb.AAPLCaseSensitive, 0x02},
		{"aapl-full-sync", smb.AAPLFullSync, 0x04},
		{"share-capabilities", uint64(smb.AdvertisedShareCapabilities), 0},
		{"share-flags", uint64(smb.AdvertisedShareFlags), 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.got != test.want {
				t.Fatalf("mask = %#x, want %#x", test.got, test.want)
			}
		})
	}
}
