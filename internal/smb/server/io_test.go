package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestIOFixtureRealHandle(t *testing.T) {
	fixture := newIOFixture(t, nil)
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	echoBody, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response := ioRoundTrip(ctx, t, client, ioMessage(session, session.NextMessageID, wire.Echo, echoBody, 1)); response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	open := insertIOOpen(t, server, session, "fixture", 3)
	if n, err := fixture.adapter.WriteAt(t.Context(), open.Handle, []byte("data"), 0); err != nil || n != 4 {
		t.Fatalf("write: %d, %v", n, err)
	}
	dst := make([]byte, 4)
	if n, err := fixture.adapter.ReadAt(t.Context(), open.Handle, dst, 0); err != nil || n != 4 || string(dst) != "data" {
		t.Fatalf("read: %d, %q, %v", n, dst, err)
	}
}
