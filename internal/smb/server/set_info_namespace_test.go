package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestBaseRenameAndDeletePreserveUnrelatedData(t *testing.T) {
	f := newNamespaceClient(t)
	f.create(t, "left", smb.KindDirectory)
	f.create(t, "right", smb.KindDirectory)
	source := f.open(t, "left/source", smb.KindFile, namespaceDeleteAccess|3, 7)
	second := f.open(t, "left/source", smb.KindFile, 3, 7)
	left := f.open(t, "left/other", smb.KindFile, 3, 7)
	right := f.open(t, "right/other", smb.KindFile, 3, 7)
	f.write(t, source, "source bytes")
	f.write(t, left, "left bytes")
	f.write(t, right, "right bytes")
	namespaceStatus(t, f.rename(t, source, "\\right\\renamed", false), smb.StatusSuccess)
	f.name(t, "left/source", 0)
	f.name(t, "right/renamed", source.Object.Inode)
	found, status := f.server.options.State.Find(source.ID, f.binding())
	namespaceStatus(t, status, smb.StatusSuccess)
	if found.Object != source.Object || found.Handle != source.Handle {
		t.Fatal("rename changed the open identity")
	}
	f.data(t, second, "source bytes")
	f.write(t, found, "renamed data")
	f.data(t, second, "renamed data")
	namespaceStatus(t, f.disposition(t, source, true), smb.StatusSuccess)
	f.name(t, "right/renamed", source.Object.Inode)
	reservation, status := f.server.options.State.Reserve(state.OpenRequest{Object: source.Object, Binding: f.binding(), Sharing: 7})
	namespaceStatus(t, status, smb.StatusDeletePending)
	if reservation != 0 {
		t.Fatal("delete-pending acquired a reservation")
	}
	f.close(t, source)
	f.name(t, "right/renamed", source.Object.Inode)
	f.close(t, second)
	f.name(t, "right/renamed", 0)
	f.name(t, "left/other", left.Object.Inode)
	f.name(t, "right/other", right.Object.Inode)
	f.data(t, left, "left bytes")
	f.data(t, right, "right bytes")
}

func TestRenameReplacementRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		dest    string
		want    smb.Status
		replace bool
	}{
		{"collision", "closed", smb.StatusObjectNameCollision, false},
		{"replace", "closed", smb.StatusSuccess, true},
		{"open destination", "open", smb.StatusAccessDenied, true},
		{"stream open destination", "stream", smb.StatusAccessDenied, true},
		{"reserved destination", "reservation", smb.StatusAccessDenied, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNamespaceClient(t)
			source := f.open(t, "source", smb.KindFile, namespaceDeleteAccess|3, 7)
			f.write(t, source, "source bytes")
			dest := f.open(t, "destination", smb.KindFile, 3, 7)
			f.write(t, dest, "destination bytes")
			if test.dest != "open" {
				f.close(t, dest)
			}
			if test.dest == "stream" {
				f.open(t, "destination:resource", smb.KindFile, 3, 7)
			}
			if test.dest == "reservation" {
				reservation, status := f.server.options.State.Reserve(state.OpenRequest{Object: dest.Object, Binding: f.binding(), Sharing: 7})
				namespaceStatus(t, status, smb.StatusSuccess)
				defer func() { namespaceStatus(t, f.server.options.State.Abort(reservation), smb.StatusSuccess) }()
			}
			namespaceStatus(t, f.rename(t, source, "destination", test.replace), test.want)
			f.data(t, source, "source bytes")
			if test.want == smb.StatusSuccess {
				f.name(t, "source", 0)
				f.name(t, "destination", source.Object.Inode)
			} else {
				f.name(t, "source", source.Object.Inode)
				f.name(t, "destination", dest.Object.Inode)
				check := f.open(t, "destination", smb.KindFile, 1, 7)
				f.data(t, check, "destination bytes")
			}
		})
	}
}

func TestDenyDeleteSharingPreventsRename(t *testing.T) {
	f := newNamespaceClient(t)
	deny := f.open(t, "source", smb.KindFile, 3, state.ShareMode(state.RightRead|state.RightWrite))
	f.write(t, deny, "unchanged")
	_, status := f.server.options.State.Reserve(state.OpenRequest{
		Object: deny.Object, Binding: f.binding(), GrantedAccess: namespaceDeleteAccess, Sharing: 7,
	})
	namespaceStatus(t, status, smb.StatusSharingViolation)
	namespaceStatus(t, f.rename(t, deny, "renamed", false), smb.StatusAccessDenied)
	namespaceStatus(t, f.disposition(t, deny, true), smb.StatusAccessDenied)
	namespaceStatus(t, f.disposition(t, deny, false), smb.StatusAccessDenied)
	f.name(t, "source", deny.Object.Inode)
	f.name(t, "renamed", 0)
	f.data(t, deny, "unchanged")
}

func TestNamedStreamRenameChangesNothing(t *testing.T) {
	f := newNamespaceClient(t)
	base := f.open(t, "source", smb.KindFile, namespaceDeleteAccess|3, 7)
	stream := f.open(t, "source:resource", smb.KindFile, namespaceDeleteAccess|3, 7)
	other := f.open(t, "source:other", smb.KindFile, 3, 7)
	f.write(t, base, "base")
	f.write(t, stream, "resource")
	f.write(t, other, "other")
	namespaceStatus(t, f.rename(t, stream, "renamed", false), smb.StatusNotSupported)
	namespaceStatus(t, f.rename(t, base, "source:new", true), smb.StatusNotSupported)
	f.name(t, "source", base.Object.Inode)
	f.name(t, "renamed", 0)
	f.data(t, base, "base")
	f.data(t, stream, "resource")
	f.data(t, other, "other")
	missing, err := f.server.options.Storage.Lookup(f.ctx, "source:new")
	if err != nil || missing.Exists {
		t.Fatalf("new stream = %+v, %v", missing, err)
	}
}

func TestDirectoryDispositionAndClearingIntent(t *testing.T) {
	f := newNamespaceClient(t)
	directory := f.open(t, "directory", smb.KindDirectory, namespaceDeleteAccess, 7)
	f.create(t, "directory/child", smb.KindFile)
	namespaceStatus(t, f.disposition(t, directory, true), smb.StatusDirectoryNotEmpty)
	// A failed emptiness check leaves the directory available to new opens.
	f.open(t, "directory", smb.KindDirectory, 0, 7)
	empty := f.open(t, "empty", smb.KindDirectory, namespaceDeleteAccess, 7)
	namespaceStatus(t, f.rename(t, empty, "moved", false), smb.StatusSuccess)
	namespaceStatus(t, f.disposition(t, empty, true), smb.StatusSuccess)
	namespaceStatus(t, f.disposition(t, empty, false), smb.StatusSuccess)
	second := f.open(t, "moved", smb.KindDirectory, 0, 7)
	namespaceStatus(t, f.disposition(t, empty, true), smb.StatusSuccess)
	f.close(t, empty)
	f.name(t, "moved", empty.Object.Inode)
	f.close(t, second)
	f.name(t, "moved", 0)
	f.name(t, "directory/child", f.create(t, "directory/child", smb.KindFile).Object.Inode)
}

func TestPendingDeleteFollowsRenamedInode(t *testing.T) {
	f := newNamespaceClient(t)
	open := f.open(t, "source", smb.KindFile, namespaceDeleteAccess|3, 7)
	f.write(t, open, "pending bytes")
	namespaceStatus(t, f.disposition(t, open, true), smb.StatusSuccess)
	namespaceStatus(t, f.rename(t, open, "renamed", false), smb.StatusSuccess)
	replacement := f.open(t, "source", smb.KindFile, 3, 7)
	f.write(t, replacement, "replacement bytes")
	f.close(t, open)
	f.name(t, "renamed", 0)
	f.name(t, "source", replacement.Object.Inode)
	f.data(t, replacement, "replacement bytes")
}

func TestNamespaceInvalidRequestsLeaveNames(t *testing.T) {
	f := newNamespaceClient(t)
	open := f.open(t, "source", smb.KindFile, namespaceDeleteAccess|3, 7)
	namespaceStatus(t, f.set(t, open, wire.ClassFileRename, []byte{1}), smb.StatusInvalidParameter)
	namespaceStatus(t, f.set(t, open, wire.ClassFileDisposition, nil), smb.StatusInvalidParameter)
	buffer, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: "destination", RootDirectory: 1})
	if err != nil {
		t.Fatal(err)
	}
	namespaceStatus(t, f.set(t, open, wire.ClassFileRename, buffer), smb.StatusNotSupported)
	namespaceStatus(t, f.rename(t, open, "../destination", false), smb.StatusObjectNameInvalid)
	namespaceStatus(t, f.rename(t, open, "missing/destination", false), smb.StatusObjectPathNotFound)
	f.name(t, "source", open.Object.Inode)
	f.name(t, "destination", 0)
	response := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, f.nextID))
	namespaceStatus(t, response[0].Header.Status, smb.StatusSuccess)
}
