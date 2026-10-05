package server

import (
	"context"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// durableTimeout is the timeout to grant a DH2Q on a regular unnamed file: the
// requested one up to 16 minutes, or 120 s for a request of 0. The open table
// grants it only with an H lease. Persistent handles are never granted.
func (contexts createContexts) durableTimeout(resolved smb.Resolved) time.Duration {
	if contexts.durable == nil || resolved.Attr.Kind != smb.KindFile || resolved.Object.Stream != "" {
		return 0
	}
	timeout := time.Duration(contexts.durable.Timeout) * time.Millisecond
	if timeout == 0 {
		return smb.DefaultDurableTimeout
	}
	return min(timeout, smb.MaxDurableTimeout)
}

// reconnectCreate hands a detached durable open back on DH2C. The client must
// match the open's identities and lease key, and the name must still lead to
// the open's file unless it is being deleted on close. Durable v1 and
// persistent reconnects find nothing.
func reconnectCreate(ctx context.Context, request RequestContext, create wire.CreateRequest, contexts createContexts) (reply, error) {
	if contexts.reconnect == nil || contexts.legacyRequest || contexts.legacyReconnect ||
		contexts.lease == nil || contexts.lease.Version != 2 || create.OplockLevel != leaseOplockLevel {
		return reply{status: smb.StatusObjectNameNotFound}, nil
	}
	if contexts.reconnect.Flags&2 != 0 {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	reconnect := state.ReconnectRequest{
		ID: state.FileID(contexts.reconnect.ID), Binding: request.Binding(),
		User: request.Session.User, Share: request.Tree.Share, ClientGUID: request.Session.ClientGUID,
		CreateGUID: contexts.reconnect.CreateGUID, LeaseKey: contexts.lease.Key,
	}
	candidate, status := request.Opens.ReconnectCandidate(reconnect)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	if !candidate.DeleteOnClose {
		resolved, unlock, err := lookupLocked(ctx, request, create.Name)
		if err != nil {
			return reply{}, err
		}
		defer unlock()
		if !resolved.Exists || resolved.Object != candidate.Object {
			return reply{status: smb.StatusObjectNameNotFound}, nil
		}
	}
	open, status := request.Opens.Reconnect(reconnect)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	// The open's handle stays in use until the attributes are read, so a
	// concurrent close cannot release it under this reply.
	open, release, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	attr, err := request.Storage.GetAttr(ctx, open.Object)
	if err != nil {
		return reply{}, err
	}
	response, err := createResponse(attr, 1) // FILE_OPENED.
	if err != nil {
		return reply{}, err
	}
	response.ID = wire.FileID(open.ID)
	if err = appendCreateContexts(request, open, state.Lease{}, &response); err != nil {
		return reply{}, err
	}
	body, err := wire.EncodeCreateResponse(response)
	return reply{body: body, fileID: response.ID}, err
}
