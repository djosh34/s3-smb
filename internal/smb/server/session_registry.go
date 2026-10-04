package server

import "context"

// The server registry contains authenticated sessions across connections.
// Never hold server.mu while acquiring a connection's sessionMu or waiting on
// work. Simultaneous replacements must not acquire each other's session locks.
func (server *Server) registerSession(id uint64, owner *connection) {
	server.mu.Lock()
	server.sessions[id] = owner
	server.mu.Unlock()
}

func (server *Server) unregisterSession(id uint64) {
	server.mu.Lock()
	delete(server.sessions, id)
	server.mu.Unlock()
}

// removeSession removes an identity, including an incomplete exchange on
// LOGOFF. A nonempty user selects only a valid session authenticated by that
// user, for PreviousSessionId. Outstanding replies keep their own key reference.
func (connection *connection) removeSession(id uint64, user string) *sessionEntry {
	connection.sessionMu.Lock()
	session := connection.sessions[id]
	if session == nil || user != "" && (!session.active || session.identity.User != user) {
		connection.sessionMu.Unlock()
		return nil
	}
	delete(connection.sessions, id)
	session.active, session.acceptor, session.trees = false, nil, nil
	connection.sessionMu.Unlock()
	connection.server.unregisterSession(id)
	return session
}

func (server *Server) replaceSession(ctx context.Context, currentID, previousID uint64, user string) error {
	if previousID == 0 || previousID == currentID {
		return nil
	}
	server.mu.Lock()
	owner := server.sessions[previousID]
	server.mu.Unlock()
	if owner == nil || owner.removeSession(previousID, user) == nil {
		return nil
	}
	// Protocol disconnect cleanup detaches durable opens before waiting for
	// old work. It is not LOGOFF, which would close durable opens too.
	actions := server.options.State.Disconnect(previousID)
	owner.stopRequests(previousID, 0, 0)
	return server.cleanup(ctx, actions)
}
