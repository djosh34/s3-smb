package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestEcho(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	messages := exchange(ctx, t, client, echo(t, 1))
	if len(messages) != 1 || messages[0].Header.Status != smb.StatusSuccess || messages[0].Header.MessageID != 1 {
		t.Fatalf("ECHO replies: %+v", messages)
	}
	if _, err := wire.DecodeEchoResponse(messages[0]); err != nil {
		t.Fatal(err)
	}
}
