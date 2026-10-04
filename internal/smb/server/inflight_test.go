package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestTreeDrainLeavesOwnAndUnrelatedIdentityHolders(t *testing.T) {
	connection := &connection{inflight: make(map[*sessionRequest]struct{})}
	var contexts []context.Context
	for _, header := range []wire.Header{
		{Command: wire.TreeDisconnect, MessageID: 1, SessionID: 1, TreeID: 1},
		{Command: wire.Read, MessageID: 2, SessionID: 1, TreeID: 2},
		{Command: wire.Read, MessageID: 3, SessionID: 2, TreeID: 1},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		operation := &sessionRequest{header: header, cancel: cancel, done: make(chan struct{})}
		connection.inflight[operation] = struct{}{}
		defer connection.finishRequest(operation)
		contexts = append(contexts, ctx)
	}
	finished := make(chan struct{})
	go func() { connection.stopRequests(1, 1, 1); close(finished) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("tree drain waited on its own or unrelated request")
	}
	for _, ctx := range contexts {
		if ctx.Err() != nil {
			t.Fatal("tree drain canceled its own or unrelated request")
		}
	}
}

func TestIdentityHolderReleasedAfterHandlerError(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Echo] = func(context.Context, RequestContext, wire.Message) (reply, error) {
		return reply{}, smb.ErrIO
	}
	connection := &connection{server: server, sessions: map[uint64]*sessionEntry{1: {active: true, identity: Session{SessionID: 1}}}}
	message := echo(t, 1)
	message.Header.SessionID = 1
	if result := connection.execute(t.Context(), message); result.status != smb.StatusIODeviceError {
		t.Fatal(result.status)
	}
	connection.sessionMu.Lock()
	remaining := len(connection.inflight)
	connection.sessionMu.Unlock()
	if remaining != 0 {
		t.Fatal("handler error retained an identity holder")
	}
}
