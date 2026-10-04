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
	if err := server.options.Storage.Remove(ctx, action.Name, action.Object.Inode); err != nil {
		return fmt.Errorf("remove closed object: %w", err)
	}
	return nil
}
