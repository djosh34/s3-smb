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

func (server *Server) cleanupAction(ctx context.Context, action state.CloseAction) error {
	if action.Remove {
		defer server.options.State.CompleteDelete(action.Object)
	}
	var result error
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := request.Storage.PathOf(ctx, action.Object.Inode)
		if errors.Is(err, smb.ErrNameNotFound) {
			return nil
		}
		if errors.Is(err, smb.ErrIdentityChanged) || errors.Is(err, smb.ErrPathNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("find deletion name: %w", err)
		}
		resolved, unlock, err := lookupLocked(ctx, request, path)
		if errors.Is(err, smb.ErrIdentityChanged) || errors.Is(err, smb.ErrPathNotFound) || errors.Is(err, smb.ErrNameNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve deletion name: %w", err)
		}
		if !resolved.Exists || resolved.Object.Inode != action.Object.Inode {
			unlock()
			continue
		}
		name := resolved.Name
		name.Stream = action.Object.Stream
		err = request.Storage.Remove(ctx, name, action.Object.Inode)
		unlock()
		if err != nil {
			return fmt.Errorf("remove closed object: %w", err)
		}
		return nil
	}
}
