package server

import (
	"math"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type openUses struct {
	drained chan struct{}
	count   uint64
}

// useOpen finds the open that id names for this request's session and tree and
// takes an active reference on it. The caller calls release exactly once.
func useOpen(request RequestContext, id wire.FileID) (open state.Open, release func(), status smb.Status) {
	// Resolve related-compound placeholders once RequestContext.FileID lands (#145).
	if id.Persistent == math.MaxUint64 && id.Volatile == math.MaxUint64 {
		return state.Open{}, nil, smb.StatusFileClosed
	}
	server := request.server
	server.openMu.Lock()
	defer server.openMu.Unlock()
	open, status = request.Opens.Find(state.FileID(id), request.Binding())
	if status != smb.StatusSuccess {
		return state.Open{}, nil, status
	}
	if server.activeOpens == nil {
		server.activeOpens = make(map[uint64]*openUses)
	}
	uses := server.activeOpens[open.ID.Persistent]
	if uses == nil {
		uses = &openUses{drained: make(chan struct{})}
		server.activeOpens[open.ID.Persistent] = uses
	}
	uses.count++
	return open, func() {
		server.openMu.Lock()
		defer server.openMu.Unlock()
		uses.count--
		if uses.count == 0 {
			delete(server.activeOpens, open.ID.Persistent)
			close(uses.drained)
		}
	}, smb.StatusSuccess
}

func (server *Server) drainOpen(id state.FileID) {
	server.openMu.Lock()
	uses := server.activeOpens[id.Persistent]
	server.openMu.Unlock()
	if uses != nil {
		<-uses.drained
	}
}
