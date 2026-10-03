package smb2

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb2/internal/erref"
)

func TestPrepareAsyncResponseCredits(t *testing.T) {
	req := &ReadRequest{Length: 1, FileId: &FileId{}}
	req.MessageId, req.CreditCharge, req.CreditRequestResponse = 10, 1, 7
	pkt := make([]byte, req.Size())
	req.Encode(pkt)
	for _, asyncID := range []uint64{0, 42} {
		for _, status := range []uint32{0, uint32(erref.STATUS_PENDING), uint32(erref.STATUS_END_OF_FILE)} {
			t.Run(fmt.Sprintf("async-%d/status-%x", asyncID, status), func(t *testing.T) {
				// Initial nonzero credits also catch stale grants on a reused header.
				rsp := PacketHeader{CreditRequestResponse: 99}
				PrepareAsyncResponse(&rsp, pkt, asyncID, status)
				want := uint16(7)
				if asyncID != 0 && status != uint32(erref.STATUS_PENDING) {
					want = 0
				}
				if rsp.CreditRequestResponse != want {
					t.Errorf("credits=%d, want %d", rsp.CreditRequestResponse, want)
				}
				if rsp.MessageId != req.MessageId || rsp.Status != status || rsp.AsyncId != asyncID {
					t.Error("response identity or status changed")
				}
				if (rsp.Flags&SMB2_FLAGS_ASYNC_COMMAND != 0) != (asyncID != 0) {
					t.Error("ASYNC flag does not match async identity")
				}
			})
		}
	}
}
