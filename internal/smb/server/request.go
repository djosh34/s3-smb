package server

import (
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

// Session is an immutable snapshot of the authenticated request identity.
// SessionID is global to this server. ClientGUID and User identify reconnects.
// Encrypted means that plaintext requests are refused after SESSION_SETUP.
type Session struct {
	User       string
	SessionID  uint64
	ClientGUID state.GUID
	Encrypted  bool
}

// Tree is an immutable snapshot of a disk-share connection owned by a session.
type Tree struct {
	Share  string
	TreeID uint32
}

// RequestContext gives handlers the validated identity and shared modules.
// Session and Tree are zero for commands that do not require those identities.
// Handlers use the separate context.Context argument for cancellation. They must
// not keep these snapshots as mutable session or tree state. Storage calls never
// run under an open-table lock. File handlers must drain active handle users
// before executing cleanup returned by Opens.
type RequestContext struct {
	server  *Server
	Storage smb.Storage
	Opens   *state.Table
	Tree    Tree
	Session Session
}

// Binding identifies the session and tree for open-table operations.
func (request RequestContext) Binding() state.Binding {
	return state.Binding{SessionID: request.Session.SessionID, TreeID: request.Tree.TreeID}
}

func (connection *connection) requestContext() RequestContext {
	return RequestContext{Storage: connection.server.options.Storage, Opens: connection.server.options.State, server: connection.server}
}
