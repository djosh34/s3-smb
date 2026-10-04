package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// sessionRequest holds a validated identity from dispatch until the handler
// returns. Registration shares sessionMu with identity removal, so work either
// joins the drain or is refused. It covers synchronous work and the local-wait
// window before an async request enters pending.
type sessionRequest struct {
	cancel context.CancelFunc
	done   chan struct{}
	header wire.Header
}

func (connection *connection) finishRequest(operation *sessionRequest) {
	if operation == nil {
		return
	}
	connection.sessionMu.Lock()
	delete(connection.inflight, operation)
	close(operation.done)
	connection.sessionMu.Unlock()
}
