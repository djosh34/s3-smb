package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

// cleanup owns the transferred actions and must finish them despite cancellation.
// Callers must release all namespace guards before entering cleanup.
func (server *Server) cleanup(ctx context.Context, actions []state.CloseAction) error {
	ctx = context.WithoutCancel(ctx)
	var result error
	for _, action := range actions {
		result = errors.Join(result, server.cleanupAction(ctx, action))
	}
	return result
}

func (server *Server) cleanupAction(ctx context.Context, action state.CloseAction) (result error) {
	defer func() { server.options.State.CleanupDone(action, result) }()
	if action.Remove {
		defer server.options.State.CompleteDelete(action.Object)
	}
	if action.Handle != nil {
		server.drainOpen(action.FileID)
		if err := server.options.Storage.Close(ctx, action.Handle); err != nil {
			result = fmt.Errorf("close open: %w", err)
		}
	}
	if action.Remove {
		result = errors.Join(result, server.removeClosed(ctx, action))
	}
	return result
}

func (server *Server) removeClosed(ctx context.Context, action state.CloseAction) error {
	request := RequestContext{server: server, Storage: server.options.Storage}
	name, unlock, err := lockName(ctx, request, action.Object)
	if errors.Is(err, smb.ErrNameNotFound) {
		return nil // Already unlinked.
	}
	if err != nil {
		return fmt.Errorf("find deletion name: %w", err)
	}
	defer unlock()
	if err = request.Storage.Remove(ctx, name, action.Object); err != nil {
		return fmt.Errorf("remove closed object: %w", err)
	}
	return nil
}
