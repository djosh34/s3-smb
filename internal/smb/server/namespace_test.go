package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

func TestParentGuardsShareServerAndAllowOtherParents(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	request := RequestContext{server: server}
	unlock := lockParent(request, 1)
	done := make(chan struct{})
	go func() {
		release := lockParent(RequestContext{server: server}, 1)
		release()
		close(done)
	}()
	other := lockParent(request, 2)
	other()
	select {
	case <-done:
		t.Error("same parent acquired twice")
	case <-time.After(10 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
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
	done := make(chan struct{}, 2)
	for _, pair := range [][2]smb.Inode{{1, 2}, {2, 1}} {
		go func() {
			for range 100 {
				unlock := lockParents(request, pair[0], pair[1])
				unlock()
			}
			done <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("opposite parent order deadlocked")
		}
	}
	unlock := lockParents(request, 1, 1)
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
