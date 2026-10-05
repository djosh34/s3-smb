package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const sessionEncryptData = 0x0004

// sessionEntry is connection-owned. sessionMu protects its mutable fields.
// Removal invalidates the identity before cleanup. savedProtection keeps keys
// for outstanding replies without keeping a logged-off session in the table.
type sessionEntry struct {
	acceptor  *auth.Acceptor
	preauth   *crypt.Preauth
	protector *crypt.Protector
	trees     map[uint32]Tree
	identity  Session
	active    bool
}

func (server *Server) allocateSessionID() (uint64, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.nextSessionID >= math.MaxUint64-1 {
		return 0, errors.New("session ID space exhausted")
	}
	server.nextSessionID++
	return server.nextSessionID, nil
}

func (server *Server) allocateTreeID() (uint32, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.nextTreeID >= math.MaxUint32-1 {
		return 0, errors.New("tree ID space exhausted")
	}
	server.nextTreeID++
	return server.nextTreeID, nil
}

func (connection *connection) sessionSetup(ctx context.Context, message wire.Message) (reply, error) {
	request, err := wire.DecodeSessionSetupRequest(message)
	if err != nil {
		return reply{}, err
	}
	// We do not advertise multichannel. Undefined flag bits are ignored.
	if request.Flags&1 != 0 {
		return reply{status: smb.StatusRequestNotAccepted}, nil
	}
	connection.sessionMu.Lock()
	result, err := connection.authenticate(message, request)
	connection.sessionMu.Unlock()
	if err != nil || result.status != smb.StatusSuccess {
		return result, err
	}
	connection.server.registerSession(result.sessionID, connection)
	if err := connection.server.replaceSession(context.WithoutCancel(ctx), result.sessionID, request.PreviousSessionID, connection.server.options.Account.User); err != nil {
		return result, fmt.Errorf("previous session cleanup: %w", err)
	}
	return result, nil
}

// authenticate runs with sessionMu held. It never waits on another connection.
func (connection *connection) authenticate(message wire.Message, request wire.SessionSetupRequest) (reply, error) {
	id := message.Header.SessionID
	var err error
	var session *sessionEntry
	if id == 0 {
		// Bound active and incomplete authentication exchanges per transport.
		if len(connection.sessions) >= 64 {
			return reply{status: smb.StatusInsufficientResources}, nil
		}
		id, err = connection.server.allocateSessionID()
		if err != nil {
			return reply{}, err
		}
		options := connection.server.options
		acceptor, authErr := auth.NewAcceptor(auth.Options{Account: options.Account, ServerName: options.ServerName, Now: options.Now})
		if authErr != nil {
			return reply{}, authErr
		}
		session = &sessionEntry{acceptor: acceptor, preauth: connection.preauth.Fork(), trees: make(map[uint32]Tree), identity: Session{SessionID: id, ClientGUID: state.GUID(connection.clientGUID)}}
		connection.sessions[id] = session
	} else {
		session = connection.sessions[id]
		if session == nil || session.acceptor == nil {
			return reply{status: smb.StatusUserSessionDeleted}, nil
		}
	}
	// SESSION_SETUP creates its reply identity during authentication, rather
	// than receiving one from decodePayload. Save it while sessionMu is held.
	connection.replyProtection[message.Header.MessageID] = savedProtection{session: session}
	session.preauth.Update(message.Raw)
	result, authErr := session.acceptor.Step(request.Token)
	if authErr != nil {
		delete(connection.sessions, id)
		connection.server.options.Logger.Info("login refused", "reason", "NTLMv2 authentication failed", "error", authErr)
		return reply{status: smb.StatusLogonFailure, sessionID: id}, nil
	}
	flags := uint16(0)
	status := smb.StatusMoreProcessingRequired
	if result.Done {
		if !connection.server.claimClient(connection) {
			delete(connection.sessions, id)
			connection.server.options.Logger.Info("login refused", "reason", "another client is logged in or has durable opens")
			return reply{status: smb.StatusRequestNotAccepted, sessionID: id}, nil
		}
		protector, protectErr := crypt.NewProtector(crypt.Options{SessionKey: result.SessionKey, Preauth: session.preauth.Sum(), SessionID: id, Cipher: connection.cipher, Signing: connection.signing, Role: crypt.RoleServer})
		if protectErr != nil {
			delete(connection.sessions, id)
			return reply{}, protectErr
		}
		session.protector, session.acceptor = protector, nil
		session.identity.User = result.User
		session.identity.Encrypted = connection.server.options.Encryption == RequireEncryption
		session.active = true
		if session.identity.Encrypted {
			flags = sessionEncryptData
		}
		status = smb.StatusSuccess
	}
	body, err := wire.EncodeSessionSetupResponse(wire.SessionSetupResponse{Token: result.Token, Flags: flags})
	return reply{body: body, status: status, sessionID: id}, err
}

func needsTree(command wire.Command) bool {
	switch uint16(command) {
	case uint16(wire.Negotiate), uint16(wire.SessionSetup), uint16(wire.Logoff), uint16(wire.TreeConnect), uint16(wire.Echo), uint16(wire.Cancel), uint16(wire.OplockBreak):
		return false
	default:
		return command <= wire.OplockBreak
	}
}

// identify finds the session and, for commands that need one, the tree that
// header names. Only LOGOFF may name a session that is still authenticating.
// The caller holds sessionMu.
func (connection *connection) identify(header wire.Header) (*sessionEntry, Tree, smb.Status) {
	session := connection.sessions[header.SessionID]
	if session == nil || !session.active && (header.Command != wire.Logoff || session.acceptor == nil) {
		return nil, Tree{}, smb.StatusUserSessionDeleted
	}
	if !needsTree(header.Command) {
		return session, Tree{}, smb.StatusSuccess
	}
	tree, exists := session.trees[header.TreeID]
	if !exists {
		return nil, Tree{}, smb.StatusNetworkNameDeleted
	}
	return session, tree, smb.StatusSuccess
}

func (connection *connection) resolveRequest(header wire.Header, cancel context.CancelFunc) (RequestContext, *sessionRequest, smb.Status) {
	request := connection.requestContext()
	if header.Command == wire.Echo && header.SessionID == 0 {
		return request, nil, smb.StatusSuccess
	}
	connection.sessionMu.Lock()
	defer connection.sessionMu.Unlock()
	session, tree, status := connection.identify(header)
	if status != smb.StatusSuccess {
		return request, nil, status
	}
	request.Session, request.Tree = session.identity, tree
	operation := &sessionRequest{header: header, cancel: cancel, done: make(chan struct{})}
	connection.inflight[operation] = struct{}{}
	return request, operation, smb.StatusSuccess
}

func (connection *connection) treeConnect(message wire.Message) (reply, error) {
	request, err := wire.DecodeTreeConnectRequest(message)
	if err != nil {
		return reply{}, err
	}
	if request.Flags != 0 {
		return reply{status: smb.StatusNotSupported}, nil
	}
	parts := strings.Split(request.Path, "\\")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "" || parts[2] == "" || parts[3] == "" || strings.ContainsAny(parts[2]+parts[3], "/") {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	if !strings.EqualFold(parts[3], connection.server.options.ShareName) {
		return reply{status: smb.StatusBadNetworkName}, nil
	}
	id, err := connection.server.allocateTreeID()
	if err != nil {
		return reply{}, err
	}
	connection.sessionMu.Lock()
	session, _, status := connection.identify(message.Header)
	if status != smb.StatusSuccess {
		connection.sessionMu.Unlock()
		return reply{status: status}, nil
	}
	session.trees[id] = Tree{TreeID: id, Share: connection.server.options.ShareName}
	connection.sessionMu.Unlock()
	body, err := wire.EncodeTreeConnectResponse(wire.TreeConnectResponse{ShareType: 1, MaximalAccess: 0x001f01ff})
	return reply{body: body, treeID: id}, err
}

// stopRequests cancels the session's or tree's requests and waits for them,
// but not for their replies to be sent: a reply can wait for the client to
// read while the client waits for the LOGOFF reply. The cleanup request itself
// may be an async related member and must not wait for its own completion.
// Other cleanup requests are cancelled but not waited on, since two cleanups
// could otherwise wait on each other.
func (connection *connection) stopRequests(sessionID uint64, treeID uint32, exceptMessageID uint64) {
	connection.sessionMu.Lock()
	var holders []*sessionRequest
	for operation := range connection.inflight {
		header := operation.header
		if (exceptMessageID == 0 || header.MessageID != exceptMessageID) && header.SessionID == sessionID && (treeID == 0 || header.TreeID == treeID) {
			holders = append(holders, operation)
		}
	}
	connection.sessionMu.Unlock()
	for _, operation := range holders {
		operation.cancel()
	}
	connection.pendingMu.Lock()
	var work []*work
	for _, pending := range connection.pending {
		if pending.header.MessageID != exceptMessageID && pending.header.SessionID == sessionID && (treeID == 0 || pending.header.TreeID == treeID) {
			pending.work.cancel()
			if pending.header.Command != wire.Logoff && pending.header.Command != wire.TreeDisconnect {
				work = append(work, pending.work)
			}
		}
	}
	connection.pendingMu.Unlock()
	for _, operation := range holders {
		if operation.header.Command != wire.Logoff && operation.header.Command != wire.TreeDisconnect {
			<-operation.done
		}
	}
	for _, operation := range work {
		<-operation.done
	}
}

func (connection *connection) logoff(ctx context.Context, message wire.Message) (reply, error) {
	id := message.Header.SessionID
	if connection.removeSession(id, "") == nil {
		return reply{status: smb.StatusUserSessionDeleted}, nil
	}
	connection.stopRequests(id, 0, message.Header.MessageID)
	if err := connection.server.cleanup(context.WithoutCancel(ctx), connection.server.options.State.CloseSession(id)); err != nil {
		return reply{}, fmt.Errorf("logoff cleanup: %w", err)
	}
	body, err := wire.EncodeLogoffResponse(wire.EmptyResponse{})
	return reply{body: body}, err
}

func (connection *connection) treeDisconnect(ctx context.Context, message wire.Message) (reply, error) {
	id, treeID := message.Header.SessionID, message.Header.TreeID
	connection.sessionMu.Lock()
	session, _, status := connection.identify(message.Header)
	if status != smb.StatusSuccess {
		connection.sessionMu.Unlock()
		return reply{status: status}, nil
	}
	delete(session.trees, treeID)
	connection.sessionMu.Unlock()
	connection.stopRequests(id, treeID, message.Header.MessageID)
	if err := connection.server.cleanup(context.WithoutCancel(ctx), connection.server.options.State.CloseTree(state.Binding{SessionID: id, TreeID: treeID})); err != nil {
		return reply{}, fmt.Errorf("tree disconnect cleanup: %w", err)
	}
	body, err := wire.EncodeTreeDisconnectResponse(wire.EmptyResponse{})
	return reply{body: body}, err
}

// detachSessions ends every session of a dropped connection, detaching its
// durable opens and closing the rest. It returns the session IDs it ended.
func (connection *connection) detachSessions() ([]uint64, []state.CloseAction) {
	connection.sessionMu.Lock()
	defer connection.sessionMu.Unlock()
	var ids []uint64
	var actions []state.CloseAction
	for id, session := range connection.sessions {
		session.active, session.acceptor, session.trees = false, nil, nil
		connection.server.unregisterSession(id)
		ids = append(ids, id)
		actions = append(actions, connection.server.options.State.Disconnect(id)...)
	}
	clear(connection.sessions)
	return ids, actions
}
