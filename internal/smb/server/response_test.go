package server

import (
	"bytes"
	"context"
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
		server, err := New(testOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		request, body := responseBodyFixture(t, test.command)
		server.handlers[test.command] = func(context.Context, RequestContext, wire.Message) (reply, error) {
			return reply{body: body, status: test.status}, nil
		}
		client, ctx := pipeClient(t, server)
		exchange(ctx, t, client, negotiateMessage(t, 1))
		response := exchange(ctx, t, client, request)[0]
		if response.Header.Status == smb.StatusPending {
			final, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			response = final.Messages[0]
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

func responseBodyFixture(t *testing.T, command wire.Command) (wire.Message, []byte) {
	t.Helper()
	var request, response []byte
	var requestErr, responseErr error
	switch uint16(command) {
	case uint16(wire.SessionSetup):
		request, requestErr = wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{})
		response, responseErr = wire.EncodeSessionSetupResponse(wire.SessionSetupResponse{Token: []byte("challenge")})
	case uint16(wire.QueryInfo):
		request, requestErr = wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{OutputLength: 16})
		response, responseErr = wire.EncodeQueryInfoResponse(wire.QueryResponse{Data: []byte("partial")})
	case uint16(wire.IOCTL):
		request, requestErr = wire.EncodeIOCTLRequest(wire.IOCTLRequest{MaxOutput: 16})
		response, responseErr = wire.EncodeIOCTLResponse(wire.IOCTLResponse{Output: []byte("partial")})
	case uint16(wire.Read):
		request, requestErr = wire.EncodeReadRequest(wire.ReadRequest{Length: 16})
		response, responseErr = wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("partial")})
	case uint16(wire.Echo):
		request, requestErr = wire.EncodeEchoRequest(wire.EmptyRequest{})
		response, responseErr = wire.EncodeEchoResponse(wire.EmptyResponse{})
	default:
		t.Fatalf("no response fixture for command %d", command)
	}
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if responseErr != nil {
		t.Fatal(responseErr)
	}
	return wire.Message{Header: wire.Header{Command: command, MessageID: 1, CreditCharge: 1, Credit: 1}, Body: request}, response
}
