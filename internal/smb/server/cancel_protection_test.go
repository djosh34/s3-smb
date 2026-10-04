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

func checkCancelProtection(t *testing.T, mode string) {
	t.Helper()
	server, release := controlledAsync(t, wire.Flush, reply{status: smb.StatusFileLockConflict}, nil)
	cipher := uint16(0)
	server.options.Encryption = AllowPlaintext
	if mode == "encrypted" || mode == "unencrypted" {
		cipher = smb.CipherAES256GCM
		server.options.Encryption = RequireEncryption
	}
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	request := asyncMessage(t, wire.Flush, session.NextMessageID)
	request.Header.SessionID, request.Header.TreeID = session.SessionID, session.TreeID
	pending := exchange(ctx, t, client, request)[0]
	cancel := cancelMessage(t, pending.Header, true)
	if mode == "signed" || mode == "encrypted" {
		if err := client.Send(ctx, []wire.Message{cancel}); err != nil {
			t.Fatal(err)
		}
	} else {
		if mode == "bad_signature" || mode == "unknown_signed_session" {
			cancel.Header.Flags |= wire.FlagSigned
			cancel.Header.Signature[0] = 1
		}
		if mode == "unknown_signed_session" {
			cancel.Header.SessionID++
		}
		payload, err := wire.Join([]wire.Message{cancel})
		if err != nil {
			t.Fatal(err)
		}
		sendPayload(ctx, t, client, payload)
	}
	accepted := mode == "unsigned" || mode == "signed" || mode == "encrypted"
	if accepted {
		final, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncFinal(t, final.Messages[0], pending, smb.StatusCancelled)
	}
	// Invalid signatures and unsigned CANCEL do not close the connection.
	// Only ECHO consumes the next sequence number or produces this reply.
	response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+1))[0]
	if response.Header.Command != wire.Echo || response.Header.Status != smb.StatusSuccess {
		t.Fatalf("CANCEL replied or dropped the connection: %+v", response.Header)
	}
	if !accepted {
		close(release)
		final, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncFinal(t, final.Messages[0], pending, smb.StatusFileLockConflict)
	}
}
