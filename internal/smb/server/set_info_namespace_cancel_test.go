package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestNamespaceCancellationWhileWaitingForGuard(t *testing.T) {
	for _, operation := range []string{"rename", "disposition"} {
		for _, held := range []string{"first", "second"} {
			t.Run(operation+"/"+held, func(t *testing.T) {
				checkNamespaceGuardCancellation(t, operation, held)
			})
		}
	}
}

func checkNamespaceGuardCancellation(t *testing.T, operation, held string) {
	t.Helper()
	f := newNamespaceClient(t)
	left := f.create(t, "left", smb.KindDirectory)
	right := f.create(t, "right", smb.KindDirectory)
	open := f.open(t, "left/source", smb.KindFile, namespaceDeleteAccess|3, 7)
	f.write(t, open, "unchanged")
	request := RequestContext{
		server: f.server, Storage: f.server.options.Storage, Opens: f.server.options.State,
		Session: Session{SessionID: f.session.SessionID}, Tree: Tree{TreeID: f.session.TreeID},
	}
	first, second := left.Object.Inode, right.Object.Inode
	if operation == "disposition" {
		second = open.Object.Inode
	}
	if first > second {
		first, second = second, first
	}
	parent := first
	if held == "second" {
		parent = second
	}
	unlock, err := lockParent(f.ctx, request, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	var buffer []byte
	if operation == "rename" {
		buffer, err = wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: "right/destination"})
	} else {
		buffer, err = wire.EncodeFileDispositionInformation(wire.FileDispositionInformation{DeletePending: true})
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan smb.Status, 1)
	go func() {
		if operation == "rename" {
			done <- setRenameInfo(ctx, request, open, buffer)
		} else {
			done <- setDispositionInfo(ctx, request, open, buffer)
		}
	}()
	waitParentUsers(t, f.server, parent, 2)
	cancel()
	select {
	case status := <-done:
		namespaceStatus(t, status, smb.StatusCancelled)
	case <-time.After(5 * time.Second):
		t.Fatal("namespace operation did not cancel while guard was held")
	}
	f.server.namespaceMu.Lock()
	guards := len(f.server.parents)
	f.server.namespaceMu.Unlock()
	if guards != 1 {
		t.Fatalf("canceled operation retained guards: %d", guards)
	}
	f.name(t, "left/source", open.Object.Inode)
	f.name(t, "right/destination", 0)
	f.data(t, open, "unchanged")
	reservation, status := f.server.options.State.Reserve(state.OpenRequest{
		Object: open.Object, Binding: f.binding(), Sharing: 7,
	})
	namespaceStatus(t, status, smb.StatusSuccess)
	namespaceStatus(t, f.server.options.State.Abort(reservation), smb.StatusSuccess)
}
