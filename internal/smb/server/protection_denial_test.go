package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSetupReplyRequiresPreauthSession(t *testing.T) {
	connection := &connection{sessions: make(map[uint64]*sessionEntry), replyProtection: make(map[uint64]savedProtection)}
	body, err := wire.EncodeSessionSetupResponse(wire.SessionSetupResponse{})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.SessionSetup, SessionID: 1, Flags: wire.FlagResponse, Status: smb.StatusMoreProcessingRequired}, Body: body}
	if _, err := connection.encodePayload([]wire.Message{message}); err == nil {
		t.Fatal("missing preauth session was accepted")
	}
}
