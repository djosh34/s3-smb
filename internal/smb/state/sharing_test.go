package state_test

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

// MS-FSA 2.1.5.1.2.2 ignores same-stream sharing modes when either
// open lacks data, execute, append and delete access.
func TestMetadataOnlySharingIsIgnored(t *testing.T) {
	for _, mask := range []uint32{0, 0x80, 0x100000, 0x100080} {
		for _, reverse := range []bool{false, true} {
			for _, committed := range []bool{false, true} {
				t.Run(fmt.Sprintf("mask_%x_reverse_%t_committed_%t", mask, reverse, committed), func(t *testing.T) {
					table := newTable(t)
					metadata, data := request(1), request(1)
					metadata.GrantedAccess, metadata.Sharing = mask, 0
					data.GrantedAccess, data.Sharing = 0x10003, 0
					first, second := metadata, data
					if reverse {
						first, second = second, first
					}
					if committed {
						commit(t, table, first, state.Grant{})
					} else {
						reserve(t, table, first)
					}
					reserve(t, table, second)
				})
			}
		}
	}
}

func sharingMask(rights uint32) uint32 {
	mask := rights & 3
	if rights&4 != 0 {
		mask |= 0x10000
	}
	return mask
}

func checkSharingRow(t *testing.T, table *state.Table, firstAccess, firstShare uint32) {
	t.Helper()
	for secondAccess := uint32(0); secondAccess < 8; secondAccess++ {
		for secondShare := uint32(0); secondShare < 8; secondShare++ {
			second := request(1)
			second.GrantedAccess, second.Sharing = sharingMask(secondAccess), state.ShareMode(secondShare)
			want := smb.StatusSuccess
			if firstAccess != 0 && secondAccess != 0 && (secondAccess&^firstShare != 0 || firstAccess&^secondShare != 0) {
				want = smb.StatusSharingViolation
			}
			token, status := table.Reserve(second)
			if status != want {
				t.Fatalf("access/share %d/%d then %d/%d: status %#x, want %#x", firstAccess, firstShare, secondAccess, secondShare, status, want)
			}
			if status == smb.StatusSuccess {
				statusIs(t, table.Abort(token), smb.StatusSuccess)
			}
		}
	}
}

func TestFullSharingMatrix(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed_%t", committed), func(t *testing.T) {
			for firstAccess := uint32(0); firstAccess < 8; firstAccess++ {
				for firstShare := uint32(0); firstShare < 8; firstShare++ {
					table := newTable(t)
					first := request(1)
					first.GrantedAccess, first.Sharing = sharingMask(firstAccess), state.ShareMode(firstShare)
					if committed {
						commit(t, table, first, state.Grant{})
					} else {
						reserve(t, table, first)
					}
					checkSharingRow(t, table, firstAccess, firstShare)
				}
			}
		})
	}
}
