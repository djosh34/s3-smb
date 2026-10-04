package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type durableContexts struct {
	request         *wire.DurableRequest
	reconnect       *wire.DurableReconnect
	lease           *wire.LeaseContext
	legacyRequest   bool
	legacyReconnect bool
}

func decodeDurableContexts(create wire.CreateRequest) (durableContexts, error) {
	var result durableContexts
	for _, c := range create.Contexts {
		switch c.Name {
		case "DH2Q":
			if result.request != nil || result.reconnect != nil {
				return result, errors.New("duplicate durable context")
			}
			value, err := wire.DecodeDurableRequest(c)
			if err != nil {
				return result, err
			}
			if value.CreateGUID == ([16]byte{}) {
				return result, errors.New("durable request has no CREATE GUID")
			}
			result.request = &value
		case "DH2C":
			if result.request != nil || result.reconnect != nil {
				return result, errors.New("duplicate durable context")
			}
			value, err := wire.DecodeDurableReconnect(c)
			if err != nil {
				return result, err
			}
			result.reconnect = &value
		case "DHnQ":
			result.legacyRequest = true
		case "DHnC":
			result.legacyReconnect = true
		case "RqLs":
			if result.lease != nil {
				return result, errors.New("duplicate lease context")
			}
			value, err := wire.DecodeLeaseContext(c)
			if err != nil {
				return result, err
			}
			result.lease = &value
		}
	}
	if result.request != nil && (result.legacyRequest || result.legacyReconnect) {
		return result, errors.New("durable v2 request conflicts with a durable v1 context")
	}
	return result, nil
}

// Both reservation and replay use this fingerprint, before any disposition can
// change the selected file. Context order is not a CREATE parameter.
func openRequestForCreate(request RequestContext, create wire.CreateRequest, object smb.ObjectKey, granted uint32) (state.OpenRequest, error) {
	open := state.OpenRequest{
		Object: object, Binding: request.Binding(), User: request.Session.User,
		Share: request.Tree.Share, ClientGUID: request.Session.ClientGUID,
		GrantedAccess: granted, Sharing: state.ShareMode(create.ShareAccess & 7),
	}
	if create.Disposition == fileSupersede || create.Options&fileDeleteOnClose != 0 {
		open.SharingIntent |= state.RightDelete
	}
	contexts, err := decodeDurableContexts(create)
	if err != nil {
		return open, err
	}
	if contexts.request != nil {
		open.CreateGUID = contexts.request.CreateGUID
	}
	create.Contexts = slices.Clone(create.Contexts)
	slices.SortFunc(create.Contexts, func(a, b wire.CreateContext) int { return strings.Compare(a.Name, b.Name) })
	body, err := wire.EncodeCreateRequest(create)
	if err != nil {
		return open, err
	}
	open.CreateParameters = sha256.Sum256(body)
	return open, nil
}

func grantCreateDurable(create wire.CreateRequest, grant *state.Grant) error {
	contexts, err := decodeDurableContexts(create)
	if err != nil {
		return err
	}
	if contexts.request == nil || grant.Directory || grant.DeleteName.Stream != "" || grant.Lease.State&smb.LeaseHandle == 0 {
		return nil
	}
	timeout := time.Duration(contexts.request.Timeout) * time.Millisecond
	if timeout == 0 {
		timeout = smb.DefaultDurableTimeout
	}
	grant.DurableTimeout = min(timeout, smb.MaxDurableTimeout)
	return nil
}

func appendCreateDurable(open state.Open, response *wire.CreateResponse) error {
	if !open.Durable {
		return nil
	}
	milliseconds := open.DurableTimeout / time.Millisecond
	if milliseconds <= 0 || milliseconds > 960000 {
		return errors.New("invalid retained durable timeout")
	}
	c, err := wire.EncodeDurableReply(wire.DurableReply{Timeout: uint32(milliseconds)})
	if err != nil {
		return err
	}
	response.Contexts = append(response.Contexts, c)
	return nil
}

// replayCreate intercepts duplicate GUIDs and DH2C before namespace selection.
// The ordinary CREATE path never sees a reconnect and never repeats a replay's
// truncation, creation, sharing reservation or lease grant.
func replayCreate(ctx context.Context, request RequestContext, message wire.Message, create wire.CreateRequest) (reply, bool, error) {
	contexts, err := decodeDurableContexts(create)
	if err != nil {
		return reply{}, true, errors.Join(smb.ErrInvalidParameter, err)
	}
	if contexts.legacyReconnect && contexts.reconnect == nil {
		return reply{status: smb.StatusObjectNameNotFound}, true, nil
	}
	if contexts.reconnect != nil {
		result, reconnectErr := reconnectCreate(ctx, request, create, contexts)
		return result, true, reconnectErr
	}
	if contexts.request == nil {
		return reply{}, false, nil
	}
	candidate, err := openRequestForCreate(request, create, smb.ObjectKey{}, expandCreateAccess(create.DesiredAccess))
	if err != nil {
		return reply{}, true, errors.Join(smb.ErrInvalidParameter, err)
	}
	original, status := request.Opens.LookupCreate(candidate)
	if status == smb.StatusObjectNameNotFound {
		return reply{}, false, nil
	}
	if message.Header.Flags&wire.FlagReplay == 0 || status == smb.StatusDuplicateObjectID {
		return reply{status: smb.StatusDuplicateObjectID}, true, nil
	}
	candidate.Object = original.Object
	open, status := request.Opens.Replay(candidate)
	if status != smb.StatusSuccess {
		return reply{status: status}, true, nil
	}
	result, err := retainedCreateReply(ctx, request, open, open.CreateAction)
	return result, true, err
}

func reconnectCreate(ctx context.Context, request RequestContext, create wire.CreateRequest, contexts durableContexts) (reply, error) {
	if contexts.reconnect.Flags&2 != 0 {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	if contexts.legacyRequest || contexts.legacyReconnect || contexts.lease == nil || contexts.lease.Version != 2 || create.OplockLevel != leaseOplockLevel {
		return reply{status: smb.StatusObjectNameNotFound}, nil
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
	open, status := reconnectBoundOpen(request, reconnect)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	return retainedCreateReply(ctx, request, open, 1) // FILE_OPENED.
}

// Session invalidation and this table publication share the owner's session
// lock. No storage call, network call or wait belongs inside that lock.
func reconnectBoundOpen(request RequestContext, reconnect state.ReconnectRequest) (state.Open, smb.Status) {
	server := request.server
	server.mu.Lock()
	owner := server.sessions[request.Session.SessionID]
	server.mu.Unlock()
	if owner == nil {
		return state.Open{}, smb.StatusUserSessionDeleted
	}
	owner.sessionMu.RLock()
	defer owner.sessionMu.RUnlock()
	session := owner.sessions[request.Session.SessionID]
	if session == nil || !session.active || session.identity != request.Session {
		return state.Open{}, smb.StatusUserSessionDeleted
	}
	if tree, exists := session.trees[request.Tree.TreeID]; !exists || tree != request.Tree {
		return state.Open{}, smb.StatusNetworkNameDeleted
	}
	return request.Opens.Reconnect(reconnect)
}

func retainedCreateReply(ctx context.Context, request RequestContext, open state.Open, action uint32) (reply, error) {
	// Cleanup cannot close the retained storage reference while attributes are
	// being read. Find also rejects a binding removed after replay or reconnect.
	open, release, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	attr, err := request.Storage.GetAttr(ctx, open.Object)
	if err != nil {
		return reply{}, err
	}
	response, err := createResponse(attr, action)
	if err != nil {
		return reply{}, err
	}
	response.ID = wire.FileID(open.ID)
	if leaseErr := appendCreateLease(request, open, &response); leaseErr != nil {
		return reply{}, leaseErr
	}
	if durableErr := appendCreateDurable(open, &response); durableErr != nil {
		return reply{}, durableErr
	}
	body, err := wire.EncodeCreateResponse(response)
	return reply{body: body, fileID: response.ID}, err
}

func encodeGrantedCreate(request RequestContext, create wire.CreateRequest, resolved smb.Resolved, open state.Open, response wire.CreateResponse) ([]byte, error) {
	if err := createLeaseResponse(request, create, resolved, open, &response); err != nil {
		return nil, err
	}
	if err := appendCreateDurable(open, &response); err != nil {
		return nil, err
	}
	return wire.EncodeCreateResponse(response)
}
