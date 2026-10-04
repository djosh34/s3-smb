package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// acknowledgeBoundLease shares identity invalidation's lock only across the pure
// table transaction. Cleanup, notification delivery and waits belong to callers
// after this function returns. A tree-less ACK needs no open-member binding.
func acknowledgeBoundLease(ctx context.Context, request RequestContext, ack wire.LeaseBreakRequest) ([]state.Break, []state.CloseAction, smb.Status, error) {
	server := request.server
	server.mu.Lock()
	owner := server.sessions[request.Session.SessionID]
	server.mu.Unlock()
	if owner == nil {
		return nil, nil, smb.StatusUserSessionDeleted, nil
	}
	owner.sessionMu.RLock()
	defer owner.sessionMu.RUnlock()
	session := owner.sessions[request.Session.SessionID]
	if session == nil || !session.active || session.identity.SessionID != request.Session.SessionID ||
		session.identity.User != request.Session.User || session.identity.ClientGUID != request.Session.ClientGUID {
		return nil, nil, smb.StatusUserSessionDeleted, nil
	}
	if request.Tree.TreeID != 0 {
		if tree, exists := session.trees[request.Tree.TreeID]; !exists || tree != request.Tree {
			return nil, nil, smb.StatusNetworkNameDeleted, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, smb.StatusSuccess, err
	}
	notifications, actions, status := request.Opens.AckBreak(state.Binding{SessionID: request.Session.SessionID}, request.Session.ClientGUID, state.GUID(ack.Key), ack.State)
	return notifications, actions, status, nil
}
