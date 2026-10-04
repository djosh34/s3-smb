package server

import "testing"

func TestCreateExpandsGenericAccess(t *testing.T) {
	for _, test := range []struct {
		name          string
		desired, want uint32
	}{
		{"read", 0x80000000, 0x00120089},
		{"write", 0x40000000, 0x00120116},
		{"execute", 0x20000000, 0x001200a0},
		{"all", 0x10000000, fileAllAccess},
		{"maximum", 0x02000000, fileAllAccess},
		{"specific", fileAppendData, fileAppendData},
		{"combined", 0xc0000000 | fileDelete, 0x0013019f},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := expandCreateAccess(test.desired); got != test.want {
				t.Fatalf("granted access = %#x, want %#x", got, test.want)
			}
		})
	}
}
