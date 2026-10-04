package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCancelSigningAndEncryption(t *testing.T) {
	for _, mode := range []string{"unsigned", "signed", "bad_signature", "unknown_signed_session", "encrypted", "unencrypted"} {
		t.Run(mode, func(t *testing.T) { checkCancelProtection(t, mode) })
	}
}

func TestUnsignedCancelInSignedCompound(t *testing.T) {
	server, _ := controlledAsync(t, wire.Flush, reply{}, nil)
	server.options.Encryption = AllowPlaintext
	client, ctx, id, protector := rawLogin(t, server)
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\server\\backup"})
	if err != nil {
		t.Fatal(err)
	}
	treeRequest := wire.Message{Header: wire.Header{Command: wire.TreeConnect, MessageID: 3, SessionID: id, CreditCharge: 1, Credit: 16}, Body: body}
	sendPayload(ctx, t, client, signMessages(t, protector, treeRequest))
	tree := receiveSignedEcho(ctx, t, client, protector)
	request := asyncMessage(t, wire.Flush, 4)
	request.Header.SessionID, request.Header.TreeID = id, tree.Header.TreeID
	sendPayload(ctx, t, client, signMessages(t, protector, request))
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending := response.Messages[0]
	if pending.Header.Status != smb.StatusPending {
		t.Fatal(pending.Header)
	}
	prefix := echo(t, 5)
	prefix.Header.SessionID, prefix.Header.Flags = id, wire.FlagSigned
	cancel := cancelMessage(t, pending.Header, true)
	cancel.Header.SessionID = ^uint64(0)
	cancel.Header.Flags |= wire.FlagRelated
	payload, err := wire.Join([]wire.Message{prefix, cancel})
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := protector.Sign(members[0].Raw)
	if err != nil {
		t.Fatal(err)
	}
	copy(payload[48:64], signature[:])
	sendPayload(ctx, t, client, payload)
	seen := make(map[uint64]bool)
	for range 2 {
		response, receiveErr := client.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if len(response.Messages) != 1 {
			t.Fatal("CANCEL added a compound response")
		}
		message := response.Messages[0]
		if verifyErr := protector.Verify(message.Raw); verifyErr != nil {
			t.Fatal(verifyErr)
		}
		if seen[message.Header.MessageID] {
			t.Fatal("duplicate response")
		}
		seen[message.Header.MessageID] = true
		switch message.Header.MessageID {
		case 4:
			assertAsyncFinal(t, message, pending, smb.StatusCancelled)
		case 5:
			if message.Header.Command != wire.Echo || message.Header.Status != smb.StatusSuccess {
				t.Fatal(message.Header)
			}
		default:
			t.Fatal(message.Header)
		}
	}
	prefix.Header.MessageID = 6
	sendPayload(ctx, t, client, signMessages(t, protector, prefix))
	if message := receiveSignedEcho(ctx, t, client, protector); message.Header.Command != wire.Echo || message.Header.MessageID != 6 {
		t.Fatal("extra cancellation reply")
	}
}
