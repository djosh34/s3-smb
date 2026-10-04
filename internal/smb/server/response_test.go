package server

import (
	"bytes"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestResponseBodiesFollowCommandStatus(t *testing.T) {
	for _, test := range []struct {
		status  smb.Status
		command wire.Command
		normal  bool
	}{
		{command: wire.SessionSetup, status: smb.StatusMoreProcessingRequired, normal: true},
		{command: wire.QueryInfo, status: smb.StatusBufferOverflow, normal: true},
		{command: wire.IOCTL, status: smb.StatusBufferOverflow, normal: true},
		{command: wire.Read, status: smb.StatusBufferOverflow, normal: true},
		{command: wire.Echo, status: smb.StatusBufferOverflow},
		{command: wire.Echo, status: smb.StatusAccessDenied},
	} {
		request, body := responseBodyFixture(t, test.command)
		response, err := makeResponse(request.Header, reply{body: body, status: test.status}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Status != test.status {
			t.Fatalf("command %d status %#x: %+v", test.command, test.status, response.Header)
		}
		if test.normal {
			if !bytes.Equal(response.Body, body) {
				t.Fatalf("command %d status %#x replaced its normal body", test.command, test.status)
			}
		} else if _, err := wire.DecodeErrorResponse(response); err != nil {
			t.Fatalf("command %d status %#x lacks an ERROR body: %v", test.command, test.status, err)
		}
	}
}
