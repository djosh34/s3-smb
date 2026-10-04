// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"slices"
	"testing"
)

func TestSMBMountpoints(t *testing.T) {
	text := `//timemachine@127.0.0.1:1445/TimeMachine on /old (smbfs, nodev)
//timemachine@127.0.0.1:54321/TimeMachine on /proxy share (smbfs, nodev)
//timemachine@127.0.0.1:54321/Other on /other (smbfs, nodev)
//timemachine@127.0.0.1:54321/TimeMachine on /incomplete
//timemachine@127.0.0.1:54321/TimeMachine on /wrong-type (apfs)
//timemachine@127.0.0.1:54321/TimeMachine on /snapshot-host (smbfs, nodev)`
	for _, test := range []struct {
		address string
		want    []string
	}{
		{"127.0.0.1:1445", []string{"/old"}},
		{"127.0.0.1:54321", []string{"/proxy share", "/snapshot-host"}},
		{"127.0.0.1:4321", nil},
	} {
		if got := SMBMountpoints(text, test.address); !slices.Equal(got, test.want) {
			t.Fatal(test.address, got, test.want)
		}
	}
}
