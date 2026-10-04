package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestDeleteOnCloseRequiresGrantedDeleteAccess(t *testing.T) {
	for _, test := range []struct {
		name            string
		options, access uint32
		want            smb.Status
	}{
		{name: "ordinary open", access: 1, want: smb.StatusSuccess},
		{name: "delete denied", options: 0x1000, access: 1, want: smb.StatusAccessDenied},
		{name: "generic mask is not a grant", options: 0x1000, access: 0x10000000, want: smb.StatusAccessDenied},
		{name: "delete granted", options: 0x1000, access: 0x10001, want: smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := checkDeleteOnClose(test.options, test.access); status != test.want {
				t.Fatalf("status = %#x, want %#x", status, test.want)
			}
		})
	}
}

type deletionStorage struct {
	pathErr   error
	lookupErr error
	cleanupStorage
	name    smb.Name
	inode   smb.Inode
	changed bool
}

func (storage *deletionStorage) Remove(_ context.Context, name smb.Name, inode smb.Inode) error {
	storage.name, storage.inode = name, inode
	return storage.removeErr
}

func (storage *deletionStorage) PathOf(context.Context, smb.Inode) (string, error) {
	return "renamed", storage.pathErr
}

func (storage *deletionStorage) Lookup(context.Context, string) (smb.Resolved, error) {
	inode := smb.Inode(2)
	if storage.changed {
		inode = 3
	}
	return smb.Resolved{Exists: true, Object: smb.ObjectKey{Inode: inode}, Name: smb.Name{Parent: 1, Base: "renamed"}}, storage.lookupErr
}

func TestCleanupRemovesCurrentNameAndSelectedStream(t *testing.T) {
	for _, stream := range []string{"", "user.AFP_Resource"} {
		t.Run(stream, func(t *testing.T) {
			options := testOptions(t)
			storage := &deletionStorage{}
			options.Storage = storage
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			name := smb.Name{Parent: 1, Base: "selected", Stream: stream}
			action := state.CloseAction{Object: smb.ObjectKey{Inode: 2, Stream: stream}, Name: name, Remove: true}
			if err := server.cleanup(t.Context(), []state.CloseAction{action}); err != nil {
				t.Fatal(err)
			}
			want := smb.Name{Parent: 1, Base: "renamed", Stream: stream}
			if storage.name != want || storage.inode != 2 {
				t.Fatalf("removed %+v inode %d, want %+v inode 2", storage.name, storage.inode, want)
			}
		})
	}
}

func TestCleanupReleasesDeletePendingOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "remove failure", "cancelled", "path failure", "lookup failure", "changed inode"} {
		t.Run(outcome, func(t *testing.T) { checkCleanupOutcome(t, outcome) })
	}
}

func checkCleanupOutcome(t *testing.T, outcome string) {
	t.Helper()
	options := testOptions(t)
	storage := cleanupOutcome(outcome)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	request := state.OpenRequest{Object: smb.ObjectKey{Inode: 2}, Binding: state.Binding{SessionID: 1, TreeID: 1}, GrantedAccess: 0x10000, Sharing: 7}
	token, status := options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := options.State.Commit(token, state.Grant{Handle: cleanupHandle{object: request.Object}, DeleteOnClose: true, DeleteName: smb.Name{Parent: 1, Base: "selected"}})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	action, status := options.State.Close(open.ID, open.Binding)
	if status != smb.StatusSuccess || !action.Remove {
		t.Fatalf("close = %+v, %#x", action, status)
	}
	if _, status = options.State.Reserve(request); status != smb.StatusDeletePending {
		t.Fatalf("cleanup window allowed reservation: %#x", status)
	}
	err = server.cleanup(t.Context(), []state.CloseAction{action})
	if (err == nil) != (outcome == "success") {
		t.Fatalf("cleanup %s: %v", outcome, err)
	}
	if outcome == "remove failure" && !errors.Is(err, smb.ErrIO) {
		t.Fatalf("remove error lost: %v", err)
	}
	if storage.changed && storage.inode != 0 {
		t.Fatal("removed a changed identity")
	}
	token, status = options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatalf("cleanup left delete-pending: %#x", status)
	}
	if status := options.State.Abort(token); status != smb.StatusSuccess {
		t.Fatal(status)
	}
}

func cleanupOutcome(outcome string) *deletionStorage {
	storage := &deletionStorage{}
	switch outcome {
	case "remove failure":
		storage.removeErr = smb.ErrIO
	case "cancelled":
		storage.removeErr = context.Canceled
	case "path failure":
		storage.pathErr = smb.ErrNameNotFound
	case "lookup failure":
		storage.lookupErr = smb.ErrIO
	case "changed inode":
		storage.changed = true
	}
	return storage
}
