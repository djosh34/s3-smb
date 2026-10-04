package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestS3PendingCompoundKeepsPrefixAndSuffix(t *testing.T) {
	fixture, proxy := newPendingIOFixture(t, false)
	server, client, ctx, session := pendingClient(t, fixture, 15*time.Second)
	open := insertIOOpen(t, server, session, "compound", 3)
	data := []byte("cold compound data")
	seedPendingData(ctx, t, fixture, open, data)
	if err := proxy.SetFault(s3fault.Fault{Method: http.MethodGet, HeaderDelay: 750 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	first := session.NextMessageID
	prefix := sessionEcho(t, session, first)
	read := pendingRead(t, session, first+1, open, 0, pendingLength(t, data))
	dependent := sessionEcho(t, session, first+2)
	dependent.Header.Flags |= wire.FlagRelated
	dependent.Header.SessionID = ^uint64(0)
	dependent.Header.TreeID = ^uint32(0)
	independent := sessionEcho(t, session, first+3)
	if err := client.Send(ctx, []wire.Message{prefix, read, dependent, independent}); err != nil {
		t.Fatal(err)
	}
	prefixReply := receivePendingIO(ctx, t, client)
	if prefixReply.Header.MessageID != first || prefixReply.Header.Status != smb.StatusSuccess || prefixReply.Header.Credit != 1 {
		t.Fatalf("prefix reply: %+v", prefixReply.Header)
	}
	readInterim := receivePendingIO(ctx, t, client)
	assertInterimIO(t, read, readInterim)
	dependentInterim := receivePendingIO(ctx, t, client)
	// Related identity is inherited before an interim is sent.
	dependent.Header.SessionID = session.SessionID
	assertInterimIO(t, dependent, dependentInterim)
	if dependentInterim.Header.AsyncID == readInterim.Header.AsyncID {
		t.Fatal("dependent member reused the read's async ID")
	}
	independentReply := receivePendingIO(ctx, t, client)
	if independentReply.Header.MessageID != first+3 || independentReply.Header.Status != smb.StatusSuccess || independentReply.Header.Credit != 1 {
		t.Fatalf("unrelated suffix did not progress: %+v", independentReply.Header)
	}
	// Final frames may arrive in either order or share a frame. The completed
	// prefix and unrelated suffix must not be sent again.
	finals := make(map[uint64]wire.Message)
	for len(finals) < 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range response.Messages {
			id := message.Header.MessageID
			if id != first+1 && id != first+2 {
				t.Fatalf("repeated prefix or unrelated suffix: %+v", message.Header)
			}
			if _, exists := finals[id]; exists {
				t.Fatalf("duplicate final: %+v", message.Header)
			}
			finals[id] = message
		}
	}
	assertFinalIO(t, readInterim, finals[first+1], smb.StatusSuccess)
	assertReadIO(t, finals[first+1], data)
	assertFinalIO(t, dependentInterim, finals[first+2], smb.StatusSuccess)
	if _, err := wire.DecodeEchoResponse(finals[first+2]); err != nil {
		t.Fatal(err)
	}
	// A further request catches any extra completion left on the connection.
	response := ioRoundTrip(ctx, t, client, sessionEcho(t, session, first+4))
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
}
