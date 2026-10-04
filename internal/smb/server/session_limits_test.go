package server

import (
	"context"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSessionAndTreeIDsDoNotWrapOrUsePlaceholders(t *testing.T) {
	server := &Server{nextSessionID: math.MaxUint64 - 2, nextTreeID: math.MaxUint32 - 2}
	if id, err := server.allocateSessionID(); err != nil || id != math.MaxUint64-1 {
		t.Fatalf("last session ID: %d %v", id, err)
	}
	if id, err := server.allocateTreeID(); err != nil || id != math.MaxUint32-1 {
		t.Fatalf("last tree ID: %d %v", id, err)
	}
	for range 2 {
		if id, err := server.allocateSessionID(); err == nil || id != 0 {
			t.Fatal("session IDs wrapped")
		}
		if id, err := server.allocateTreeID(); err == nil || id != 0 {
			t.Fatal("tree IDs wrapped")
		}
	}
}

func TestConnectionBoundsIncompleteSessionExchanges(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	connection := &connection{server: server, sessions: make(map[uint64]*sessionEntry), preauth: crypt.NewPreauth()}
	for id := uint64(1); id <= 64; id++ {
		connection.sessions[id] = &sessionEntry{}
	}
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := connection.sessionSetup(wire.Message{Header: wire.Header{Command: wire.SessionSetup}, Body: body})
	if err != nil || result.status != smb.StatusInsufficientResources || len(connection.sessions) != 64 {
		t.Fatalf("session limit: %+v %v", result, err)
	}
}

func TestTreeConnectCannotPublishOnInactiveSession(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	connection := &connection{server: server, sessions: map[uint64]*sessionEntry{1: {identity: Session{SessionID: 1}, trees: make(map[uint32]Tree)}}}
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\server\\backup"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := connection.treeConnect(wire.Message{Header: wire.Header{Command: wire.TreeConnect, SessionID: 1}, Body: body})
	if err != nil || result.status != smb.StatusUserSessionDeleted || len(connection.sessions[1].trees) != 0 {
		t.Fatalf("stale tree grant: %+v %v", result, err)
	}
}

func TestCanceledRequestDoesNotDispatch(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Echo] = func(context.Context, RequestContext, wire.Message) (reply, error) {
		t.Error("canceled request reached handler")
		return reply{}, nil
	}
	connection := &connection{server: server}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := connection.execute(ctx, echo(t, 1), compoundState{}); result.status != smb.StatusCancelled {
		t.Fatal(result)
	}
}
