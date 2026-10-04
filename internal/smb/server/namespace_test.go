package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestParentGuardsShareServerAndAllowOtherParents(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	request := RequestContext{server: server}
	unlock, err := lockParent(t.Context(), request, 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		release, lockErr := lockParent(t.Context(), RequestContext{server: server}, 1)
		if lockErr == nil {
			release()
		}
		done <- lockErr
	}()
	other, err := lockParent(t.Context(), request, 2)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	other()
	select {
	case <-done:
		t.Error("same parent acquired twice")
	case <-time.After(10 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parent guard did not release")
	}
	if len(server.parents) != 0 {
		t.Fatal("unused parent guards retained")
	}
}

func TestLockParentsOrdersAndDeduplicates(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	request := RequestContext{server: server}
	done := make(chan error, 2)
	for _, pair := range [][2]smb.Inode{{1, 2}, {2, 1}} {
		go func() {
			for range 100 {
				unlock, lockErr := lockParents(t.Context(), request, pair[0], pair[1])
				if lockErr != nil {
					done <- lockErr
					return
				}
				unlock()
			}
			done <- nil
		}()
	}
	for range 2 {
		select {
		case lockErr := <-done:
			if lockErr != nil {
				t.Fatal(lockErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("opposite parent order deadlocked")
		}
	}
	unlock, err := lockParents(t.Context(), request, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestCloseOpenUsesCleanupAndRejectsClosedID(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	open := insertSessionOpen(t, server, smbtest.Session{SessionID: 1, TreeID: 2}, false, 2)
	request := openRequestContext(server, open)
	if err := closeOpen(context.Background(), request, open.ID); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 1 {
		t.Fatal("close did not clean up")
	}
	if err := closeOpen(context.Background(), request, open.ID); !errors.Is(err, smb.ErrInvalidHandle) {
		t.Fatalf("closed ID: %v", err)
	}
	if _, status := request.Opens.Find(open.ID, request.Binding()); status != smb.StatusFileClosed {
		t.Fatal("close did not remove table entry")
	}
}

func TestLookupLockedReturnsSelectionUnderGuard(t *testing.T) {
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	request := RequestContext{server: server, Storage: options.Storage}
	selected, unlock, err := lookupLocked(context.Background(), request, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if !selected.Exists || selected.Object != (smb.ObjectKey{Inode: 2}) {
		t.Fatalf("wrong selection: %+v", selected)
	}
}

func TestLookupLockedRetriesChangedParent(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var calls uint64
	storage := &lookupStorage{lookup: func(context.Context, string) (smb.Resolved, error) {
		calls++
		parent := smb.Inode(2)
		if calls == 1 {
			parent = 1
		}
		return smb.Resolved{Name: smb.Name{Parent: parent}, Object: smb.ObjectKey{Inode: smb.Inode(calls)}}, nil
	}}
	request := RequestContext{server: server, Storage: storage}
	selected, unlock, err := lookupLocked(context.Background(), request, "file")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if calls != 4 || selected.Object.Inode != 4 || selected.Name.Parent != 2 {
		t.Fatalf("selection did not retry: %+v, calls %d", selected, calls)
	}
	if len(server.parents) != 0 {
		t.Fatal("retry retained parent guard")
	}
}

func TestLookupLockedErrorsReleaseGuard(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		server, err := New(testOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		storage := &lookupStorage{lookup: func(context.Context, string) (smb.Resolved, error) {
			calls++
			if calls == failAt {
				return smb.Resolved{}, smb.ErrIO
			}
			return smb.Resolved{Name: smb.Name{Parent: 1}}, nil
		}}
		_, unlock, err := lookupLocked(context.Background(), RequestContext{server: server, Storage: storage}, "file")
		if !errors.Is(err, smb.ErrIO) || unlock != nil || len(server.parents) != 0 {
			t.Fatalf("lookup error: %v, unlock %t, guards %d", err, unlock != nil, len(server.parents))
		}
	}
}

func TestLookupLockedCancellationWhileWaitingForParent(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	request := RequestContext{server: server, Storage: &cleanupStorage{}}
	unlock, err := lockParent(t.Context(), request, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, release, err := lookupLocked(ctx, request, "file")
		if release != nil {
			release()
		}
		done <- err
	}()
	waitParentUsers(t, server, 1, 2)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled lookup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not cancel while parent held")
	}
}

func TestCloseOpenDoesNotDrainWhileHoldingParent(t *testing.T) {
	options := testOptions(t)
	selected := make(chan struct{})
	allowClose := make(chan struct{})
	storage := &lookupStorage{}
	var calls atomic.Int32
	storage.lookup = func(ctx context.Context, path string) (smb.Resolved, error) {
		if calls.Add(1) == 2 {
			close(selected)
			<-allowClose
		}
		return storage.cleanupStorage.Lookup(ctx, path)
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	open := insertSessionOpen(t, server, smbtest.Session{SessionID: 1, TreeID: 2}, false, 2)
	request := openRequestContext(server, open)
	_, release, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	done := make(chan error, 1)
	go func() { done <- closeOpen(context.Background(), request, open.ID) }()
	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not acquire parent")
	}
	go func() {
		unlock, err := lockParent(t.Context(), request, 1)
		release()
		if err != nil {
			t.Error(err)
			return
		}
		unlock()
	}()
	waitParentUsers(t, server, 1, 2)
	close(allowClose)
	select {
	case err := <-done:
		if err != nil || storage.closed.Load() != 1 {
			t.Fatalf("close: %v, handles closed %d", err, storage.closed.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close drained with parent guard held")
	}
}

func TestCleanupDeletionBlocksSameParentLookup(t *testing.T) {
	options := testOptions(t)
	storage := &removingStorage{entered: make(chan struct{}), proceed: make(chan struct{})}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	action := state.CloseAction{Object: smb.ObjectKey{Inode: 2}, Remove: true}
	cleanup := make(chan error, 1)
	go func() { cleanup <- server.cleanup(context.Background(), []state.CloseAction{action}) }()
	select {
	case <-storage.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not reach deletion")
	}
	request := RequestContext{server: server, Storage: storage}
	lookup := make(chan error, 1)
	go func() {
		_, unlock, lookupErr := lookupLocked(context.Background(), request, "file")
		if unlock != nil {
			unlock()
		}
		lookup <- lookupErr
	}()
	waitParentUsers(t, server, 1, 2)
	other, err := lockParent(t.Context(), request, 2)
	if err != nil {
		t.Fatal(err)
	}
	other()
	select {
	case err := <-lookup:
		t.Fatalf("lookup bypassed deletion guard: %v", err)
	default:
	}
	close(storage.proceed)
	for _, done := range []<-chan error{cleanup, lookup} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("namespace operation did not finish")
		}
	}
}
