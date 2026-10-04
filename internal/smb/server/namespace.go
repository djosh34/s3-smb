package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

type parentGuard struct {
	held chan struct{}
	refs uint64
}

func (server *Server) parentGuard(parent smb.Inode) *parentGuard {
	server.namespaceMu.Lock()
	defer server.namespaceMu.Unlock()
	if server.parents == nil {
		server.parents = make(map[smb.Inode]*parentGuard)
	}
	guard := server.parents[parent]
	if guard == nil {
		guard = &parentGuard{held: make(chan struct{}, 1)}
		server.parents[parent] = guard
	}
	guard.refs++
	return guard
}

func (server *Server) dropParent(parent smb.Inode, guard *parentGuard) {
	server.namespaceMu.Lock()
	defer server.namespaceMu.Unlock()
	guard.refs--
	if guard.refs == 0 {
		delete(server.parents, parent)
	}
}

// lockParent locks namespace operations in one parent until the returned unlock.
// Handlers may take this guard while holding useOpen. Release all namespace
// guards before Cleanup, which drains references and takes deletion guards.
func lockParent(ctx context.Context, request RequestContext, parent smb.Inode) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	guard := request.server.parentGuard(parent)
	select {
	case guard.held <- struct{}{}:
		return func() {
			<-guard.held
			request.server.dropParent(parent, guard)
		}, nil
	case <-ctx.Done():
		request.server.dropParent(parent, guard)
		return nil, ctx.Err()
	}
}

// lockParents locks both parents in inode order, or locks once if they match.
func lockParents(ctx context.Context, request RequestContext, first, second smb.Inode) (func(), error) {
	if first > second {
		first, second = second, first
	}
	unlockFirst, err := lockParent(ctx, request, first)
	if err != nil {
		return nil, err
	}
	if first == second {
		return unlockFirst, nil
	}
	unlockSecond, err := lockParent(ctx, request, second)
	if err != nil {
		unlockFirst()
		return nil, err
	}
	return func() {
		unlockSecond()
		unlockFirst()
	}, nil
}

// lookupLocked selects a name under its parent guard. The caller unlocks once.
// The first lookup discovers only the parent; a rename that changes it makes
// lookupLocked look again, at most nameTries times.
func lookupLocked(ctx context.Context, request RequestContext, path string) (smb.Resolved, func(), error) {
	for range nameTries {
		if err := ctx.Err(); err != nil {
			return smb.Resolved{}, nil, err
		}
		discovered, err := request.Storage.Lookup(ctx, path)
		if err != nil {
			return smb.Resolved{}, nil, err
		}
		unlock, err := lockParent(ctx, request, discovered.Name.Parent)
		if err != nil {
			return smb.Resolved{}, nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			unlock()
			return smb.Resolved{}, nil, ctxErr
		}
		selected, err := request.Storage.Lookup(ctx, path)
		if err != nil {
			unlock()
			return smb.Resolved{}, nil, err
		}
		if selected.Name.Parent == discovered.Name.Parent {
			return selected, unlock, nil
		}
		unlock()
	}
	return smb.Resolved{}, nil, fmt.Errorf("parent of %q kept changing: %w", path, smb.ErrIdentityChanged)
}

func closeOpen(ctx context.Context, request RequestContext, id state.FileID) error {
	action, err := removeOpen(ctx, request, id)
	if action.FileID == (state.FileID{}) {
		return err
	}
	return errors.Join(err, request.Cleanup(ctx, []state.CloseAction{action}))
}

// removeOpen transfers cleanup after table removal and namespace unlock. A
// discovery error can accompany a valid action; the caller must still clean it.
// Callers release their own useOpen reference before removing the open.
func removeOpen(ctx context.Context, request RequestContext, id state.FileID) (state.CloseAction, error) {
	open, status := request.Opens.Find(id, request.Binding())
	if status != smb.StatusSuccess {
		return state.CloseAction{}, smb.ErrInvalidHandle
	}
	_, unlock, lookupErr := lockName(ctx, request, open.Object.Inode)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if unlock != nil {
			unlock()
		}
		return state.CloseAction{}, errors.Join(lookupErr, ctxErr)
	}
	if errors.Is(lookupErr, context.Canceled) || errors.Is(lookupErr, context.DeadlineExceeded) {
		return state.CloseAction{}, lookupErr
	}
	if errors.Is(lookupErr, smb.ErrNameNotFound) {
		lookupErr = nil // An unlinked inode has no namespace name to guard.
	}
	action, status := request.Opens.Close(id, request.Binding())
	if unlock != nil {
		unlock()
	}
	if status != smb.StatusSuccess {
		return state.CloseAction{}, errors.Join(lookupErr, smb.ErrInvalidHandle)
	}
	return action, lookupErr
}

// nameTries bounds how often a lookup retries a name that a concurrent rename
// keeps moving. Storage that keeps failing this way gets an error.
const nameTries = 4

// lockName finds the current name of inode and locks its parent until the
// returned unlock. An unlinked inode returns ErrNameNotFound. A rename between
// finding and locking the name makes it look again, at most nameTries times.
func lockName(ctx context.Context, request RequestContext, inode smb.Inode) (smb.Name, func(), error) {
	for range nameTries {
		path, err := request.Storage.PathOf(ctx, inode)
		if errors.Is(err, smb.ErrIdentityChanged) || errors.Is(err, smb.ErrPathNotFound) {
			continue
		}
		if err != nil {
			return smb.Name{}, nil, err
		}
		selected, unlock, err := lookupLocked(ctx, request, path)
		if errors.Is(err, smb.ErrIdentityChanged) || errors.Is(err, smb.ErrPathNotFound) || errors.Is(err, smb.ErrNameNotFound) {
			continue
		}
		if err != nil {
			return smb.Name{}, nil, err
		}
		if selected.Exists && selected.Object.Inode == inode {
			return selected.Name, unlock, nil
		}
		unlock()
	}
	return smb.Name{}, nil, fmt.Errorf("name of inode %d kept changing: %w", inode, smb.ErrIdentityChanged)
}
