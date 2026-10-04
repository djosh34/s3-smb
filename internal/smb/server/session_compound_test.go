package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestRelatedCleanupDoesNotWaitForItsOwnCompletion(t *testing.T) {
	for _, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		t.Run(fmt.Sprintf("command_%d", command), func(t *testing.T) { checkRelatedCleanup(t, command) })
	}
}

func TestSendRawBypassesLoginProtection(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	payload, err := wire.Join([]wire.Message{sessionEcho(t, session, session.NextMessageID)})
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if response.Messages[0].Header.Status != smb.StatusAccessDenied {
		t.Fatal("raw plaintext was encrypted by the client or accepted by the server")
	}
	if response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+1))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("raw refusal closed the connection")
	}
}
