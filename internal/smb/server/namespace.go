package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

type parentGuard struct {
	mu   sync.Mutex
	refs uint64
}

// lockParent locks namespace operations in one parent until the returned unlock.
func lockParent(request RequestContext, parent smb.Inode) func() {
	server := request.server
	server.namespaceMu.Lock()
	if server.parents == nil {
		server.parents = make(map[smb.Inode]*parentGuard)
	}
	guard := server.parents[parent]
	if guard == nil {
		guard = &parentGuard{}
		server.parents[parent] = guard
	}
	guard.refs++
	server.namespaceMu.Unlock()
	guard.mu.Lock()
	return func() {
		server.namespaceMu.Lock()
		guard.refs--
		if guard.refs == 0 {
			delete(server.parents, parent)
		}
		guard.mu.Unlock()
		server.namespaceMu.Unlock()
	}
}

// lockParents locks both parents in inode order, or locks once if they match.
func lockParents(request RequestContext, first, second smb.Inode) func() {
	if first > second {
		first, second = second, first
	}
	unlockFirst := lockParent(request, first)
	if first == second {
		return unlockFirst
	}
	unlockSecond := lockParent(request, second)
	return func() {
		unlockSecond()
		unlockFirst()
	}
}

// lookupLocked selects a name under its parent guard. The caller unlocks once.
// The first lookup discovers only the parent; changes to it require a retry.
func lookupLocked(ctx context.Context, request RequestContext, path string) (smb.Resolved, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return smb.Resolved{}, nil, err
		}
		discovered, err := request.Storage.Lookup(ctx, path)
		if err != nil {
			return smb.Resolved{}, nil, err
		}
		unlock := lockParent(request, discovered.Name.Parent)
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
}

// closeOpen holds the current parent guard from table removal through cleanup.
// Callers release their own useOpen reference before calling it.
func closeOpen(ctx context.Context, request RequestContext, id state.FileID) error {
	open, status := request.Opens.Find(id, request.Binding())
	if status != smb.StatusSuccess {
		return smb.ErrInvalidHandle
	}
	for {
		path, err := request.Storage.PathOf(ctx, open.Object.Inode)
		if err != nil {
			return fmt.Errorf("find closing name: %w", err)
		}
		selected, unlock, err := lookupLocked(ctx, request, path)
		if err != nil {
			return fmt.Errorf("resolve closing name: %w", err)
		}
		if !selected.Exists || selected.Object.Inode != open.Object.Inode {
			unlock()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			continue
		}
		action, status := request.Opens.Close(id, request.Binding())
		if status != smb.StatusSuccess {
			unlock()
			return smb.ErrInvalidHandle
		}
		err = request.server.cleanupAction(context.WithoutCancel(ctx), action, &selected)
		unlock()
		return err
	}
}
