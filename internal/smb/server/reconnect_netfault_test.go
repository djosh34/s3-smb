package server

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReconnectNetfaultFixture(t *testing.T) {
	fixture := newReconnectFixture(t, smb.CipherAES128GCM)
	client, session, transport := fixture.client(t, fixture.login.ClientGUID)
	open := insertIOOpen(t, fixture.server, session, "band", fileReadData|fileWriteData)
	id := wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}
	for _, command := range []wire.Command{wire.Write, wire.Read, wire.Flush} {
		message := reconnectIO(t, &session, command, id, []byte("acknowledged"), 0)
		if reply := ioRoundTrip(fixture.ctx, t, client, message); reply.Header.Status != smb.StatusSuccess {
			t.Fatal(reply.Header)
		}
	}
	gate := fixture.storage.arm(wire.Write)
	message := reconnectIO(t, &session, wire.Write, id, []byte("unanswered"), 0)
	if err := client.Send(fixture.ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	awaitReconnectEvent(fixture.ctx, t, gate.entered)
	if err := transport.proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	awaitReconnectEvent(fixture.ctx, t, gate.canceled)
	awaitReconnectEvent(fixture.ctx, t, transport.done)
	fixture.clock.advance(30 * time.Second)
}
