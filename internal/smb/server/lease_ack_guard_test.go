package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Retire an identity after dispatch has retained its immutable request snapshot.
// The ACK transaction must revalidate that snapshot before changing lease state.
func TestLeaseAckRevalidatesIdentityBeforeMutation(t *testing.T) {
	for _, mode := range []string{"removed session", "inactive session", "removed tree", "canceled"} {
		t.Run(mode, func(t *testing.T) { checkLeaseAckIdentityGuard(t, mode) })
	}
}

func checkLeaseAckIdentityGuard(t *testing.T, mode string) {
	t.Helper()
	server, holder, _ := newCreateLeaseClients(t)
	other := loginCreateLeaseClient(t, server, 2)
	opened := holder.create(t, leaseCreateRequest("guarded-ack"), leaseV2(4, 7))
	open, status := server.options.State.Find(state.FileID(opened.Reply.ID), state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	done := startServerBreak(holder.ctx, server, open, smb.LeaseRead|smb.LeaseHandle)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := server.sessionConnection(other.session.SessionID)
	if owner == nil {
		t.Fatal("second login has no canonical owner")
	}
	ctx, cancel := context.WithCancel(other.ctx)
	defer cancel()
	header := wire.Header{Command: wire.OplockBreak, SessionID: other.session.SessionID, MessageID: other.next}
	request, operation, status := owner.resolveRequest(header, cancel)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	defer owner.finishRequest(operation)
	want := smb.StatusUserSessionDeleted
	switch mode {
	case "removed session":
		owner.removeSession(other.session.SessionID, "")
	case "inactive session":
		owner.sessionMu.Lock()
		owner.sessions[other.session.SessionID].active = false
		owner.sessionMu.Unlock()
	case "removed tree":
		owner.sessionMu.Lock()
		request.Tree = owner.sessions[other.session.SessionID].trees[other.session.TreeID]
		delete(owner.sessions[other.session.SessionID].trees, other.session.TreeID)
		owner.sessionMu.Unlock()
		want = smb.StatusNetworkNameDeleted
	case "canceled":
		cancel()
	}
	body, err := wire.EncodeLeaseBreakRequest(wire.LeaseBreakRequest{Key: notification.Key, State: notification.NewState})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handleOplockBreak(ctx, request, wire.Message{Header: header, Body: body})
	if mode == "canceled" {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled ACK = %+v, %v", result, err)
		}
	} else if err != nil || result.status != want {
		t.Fatalf("retired ACK = %#x, %v; want %#x", result.status, err, want)
	}
	current, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || !current.Breaking || current.State != 7 || current.BreakTo != 3 || current.Epoch != notification.Epoch {
		t.Fatalf("refused ACK changed captured break: %+v", current)
	}
	holder.ack(t, notification)
	finishServerBreak(holder.ctx, t, done)
}
