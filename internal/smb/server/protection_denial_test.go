package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func checkPlaintextDenial(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector, response []byte, receiveErr error, message wire.Message, calls *atomic.Int32) {
	t.Helper()
	if receiveErr != nil {
		t.Fatal(receiveErr)
	}
	plain, err := protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Header.Status != smb.StatusAccessDenied || calls.Load() != 0 {
		t.Fatal("plaintext did not get ACCESS_DENIED before dispatch")
	}
	message.Header.MessageID++
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	payload, err = protector.Seal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(ctx, t, client, payload)
	response, err = client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err = wire.Split(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Header.Status != smb.StatusSuccess || calls.Load() != 1 {
		t.Fatal("plaintext refusal closed the connection")
	}
}

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
