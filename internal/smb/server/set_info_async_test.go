package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSetInfoBufferedMetadataPendingIssue428(t *testing.T) {
	for _, test := range []struct {
		name  string
		class wire.FileInfoClass
	}{
		{"basic times", wire.ClassFileBasic},
		{"end of file", wire.ClassFileEndOfFile},
		{"allocation", wire.ClassFileAllocation},
	} {
		for _, fail := range []bool{false, true} {
			outcome := "success"
			var failure error
			if fail {
				outcome, failure = "error", smb.ErrIO
			}
			t.Run(test.name+"/"+outcome, func(t *testing.T) {
				checkSetInfoBufferedPending(t, test.class, failure)
			})
		}
	}
}

func TestFastSetInfoAllocationStaysSynchronousIssue428(t *testing.T) {
	f := newSetInfoFixture(t, 2)
	f.write(t, "buffered data")
	before := f.attr(t)
	request := setInfoAsyncRequest(t, f, wire.ClassFileAllocation)
	responses := exchange(f.ctx, t, f.client, request)
	if len(responses) != 1 {
		t.Fatalf("local SET_INFO replies: %+v", responses)
	}
	response := responses[0]
	header := response.Header
	if header.Command != wire.SetInfo || header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.TreeID != request.Header.TreeID || header.CreditCharge != 1 || header.Credit != 16 {
		t.Fatalf("local SET_INFO identity/credits: %+v", header)
	}
	if _, err := wire.DecodeSetInfoResponse(response); err != nil {
		t.Fatal(err)
	}
	if after := f.attr(t); after != before {
		t.Fatalf("allocation growth hint changed metadata: before %+v, after %+v", before, after)
	}
	assertSetInfoAsyncEcho(t, f, request.Header.MessageID+1)
	assertSetInfoQuiet(t, f)
}
