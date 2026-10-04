package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
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
// run under an open-table lock. Cleanup drains active handle users before
// executing actions returned by Opens. Resolve request IDs with FileID,
// then set reply.fileID to the ID used or created so related members inherit it.
type RequestContext struct {
	server  *Server
	Storage smb.Storage
	Opens   *state.Table
	Tree    Tree
	Session Session

	fileID  wire.FileID
	related bool
}

// FileID resolves the all-bits-set placeholder only for related members.
// Without a saved ID it returns STATUS_INVALID_PARAMETER. Other IDs pass through.
func (request RequestContext) FileID(id wire.FileID) (wire.FileID, smb.Status) {
	if request.related && id == (wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}) {
		if request.fileID == (wire.FileID{}) {
			return wire.FileID{}, smb.StatusInvalidParameter
		}
		return request.fileID, smb.StatusSuccess
	}
	return id, smb.StatusSuccess
}

// Cleanup drains active references, then closes and deletes transferred actions
// even if ctx is cancelled. Callers must hold no namespace guard; cleanup
// acquires deletion guards.
func (request RequestContext) Cleanup(ctx context.Context, actions []state.CloseAction) error {
	return request.server.cleanup(ctx, actions)
}

// Binding identifies the session and tree for open-table operations.
func (request RequestContext) Binding() state.Binding {
	return state.Binding{SessionID: request.Session.SessionID, TreeID: request.Tree.TreeID}
}

func (connection *connection) requestContext() RequestContext {
	return RequestContext{Storage: connection.server.options.Storage, Opens: connection.server.options.State, server: connection.server}
}
