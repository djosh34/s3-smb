package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func (server *Server) cleanup(ctx context.Context, actions []state.CloseAction) error {
	var result error
	for _, action := range actions {
		result = errors.Join(result, server.cleanupAction(ctx, action))
	}
	return result
}

func (server *Server) cleanupAction(ctx context.Context, action state.CloseAction) error {
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
	path, err := server.options.Storage.PathOf(ctx, action.Object.Inode)
	if err != nil {
		return fmt.Errorf("find deletion name: %w", err)
	}
	request := RequestContext{server: server, Storage: server.options.Storage}
	resolved, unlock, err := lookupLocked(ctx, request, path)
	if err != nil {
		return fmt.Errorf("resolve deletion name: %w", err)
	}
	defer unlock()
	return server.removeSelected(ctx, action, resolved)
}

func (server *Server) removeSelected(ctx context.Context, action state.CloseAction, resolved smb.Resolved) error {
	if !resolved.Exists || resolved.Object.Inode != action.Object.Inode {
		return errors.New("deletion name no longer identifies the closed inode")
	}
	name := resolved.Name
	name.Stream = action.Object.Stream
	if err := server.options.Storage.Remove(ctx, name, action.Object.Inode); err != nil {
		return fmt.Errorf("remove closed object: %w", err)
	}
	return nil
}
