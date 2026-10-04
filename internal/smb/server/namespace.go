package server

import (
	"context"
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
// Handlers may take this guard while holding useOpen. Cleanup must never wait
// for active references while holding a namespace guard.
func lockParent(request RequestContext, parent smb.Inode) func() {
	guard := request.server.parentGuard(parent)
	guard.held <- struct{}{}
	return func() {
		<-guard.held
		request.server.dropParent(parent, guard)
	}
}

func lockParentContext(ctx context.Context, request RequestContext, parent smb.Inode) (func(), error) {
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
		unlock, err := lockParentContext(ctx, request, discovered.Name.Parent)
		if err != nil {
			return smb.Resolved{}, nil, err
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
}

// closeOpen guards table removal, then unlocks before draining active references.
// Cleanup locks the current parent again for deletion. Delete-pending must cover
// that gap (#370). Callers release their own useOpen reference before calling it.
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
		unlock()
		return request.server.cleanup(context.WithoutCancel(ctx), []state.CloseAction{action})
	}
}
