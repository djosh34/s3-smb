package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReconnectPublicationRejectsInvalidatedBinding(t *testing.T) {
	for _, sessionRemoved := range []bool{false, true} {
		name := "tree disconnect"
		if sessionRemoved {
			name = "session removal"
		}
		t.Run(name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			options := durableCreateOptions()
			first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
			server.mu.Lock()
			owner := server.sessions[session.SessionID]
			server.mu.Unlock()
			request, status := owner.resolveRequest(wire.Header{Command: wire.Create, SessionID: session.SessionID, TreeID: session.TreeID})
			if status != smb.StatusSuccess {
				t.Fatalf("resolve = %#x", status)
			}
			server.options.State.Disconnect(session.SessionID)
			reconnect := state.ReconnectRequest{ID: state.FileID(first.Reply.ID), Binding: request.Binding(), User: request.Session.User, Share: request.Tree.Share, ClientGUID: request.Session.ClientGUID, CreateGUID: options.Durable.CreateGUID, LeaseKey: options.Lease.Key}
			candidate, status := server.options.State.ReconnectCandidate(reconnect)
			if status != smb.StatusSuccess {
				t.Fatalf("candidate = %#x", status)
			}
			// Pause the handler after its candidate check, then invalidate the
			// binding exactly as lifecycle cleanup does before table cleanup.
			want := smb.StatusNetworkNameDeleted
			if sessionRemoved {
				owner.removeSession(session.SessionID, "")
				want = smb.StatusUserSessionDeleted
			} else {
				owner.sessionMu.Lock()
				delete(owner.sessions[session.SessionID].trees, session.TreeID)
				owner.sessionMu.Unlock()
			}
			_, status = reconnectBoundOpen(request, reconnect)
			if status != want {
				t.Fatalf("publication after invalidation = %#x, want %#x", status, want)
			}
			retained, status := server.options.State.ReconnectCandidate(reconnect)
			if status != smb.StatusSuccess || retained != candidate {
				t.Fatalf("invalidated handler changed retained open: %+v, %#x", retained, status)
			}
		})
	}
}
