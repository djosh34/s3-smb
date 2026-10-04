package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb/state"
)

func (server *Server) cleanup(ctx context.Context, actions []state.CloseAction) error {
	var result error
	for _, action := range actions {
		if action.Handle != nil {
			server.drainOpen(action.FileID)
			if err := server.options.Storage.Close(ctx, action.Handle); err != nil {
				result = errors.Join(result, fmt.Errorf("close open: %w", err))
			}
		}
		if action.Remove {
			result = errors.Join(result, server.removeClosed(ctx, action))
		}
	}
	return result
}

func (server *Server) removeClosed(ctx context.Context, action state.CloseAction) error {
	path, err := server.options.Storage.PathOf(ctx, action.Object.Inode)
	if err != nil {
		return fmt.Errorf("find deletion name: %w", err)
	}
	resolved, err := server.options.Storage.Lookup(ctx, path)
	if err != nil {
		return fmt.Errorf("resolve deletion name: %w", err)
	}
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
