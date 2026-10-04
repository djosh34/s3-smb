package server

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const namespaceDeleteAccess uint32 = 0x00010000

// namespaceClient uses real storage and sends every mutation through SET_INFO.
type namespaceClient struct {
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	nextID  uint64
}

func newNamespaceClient(t *testing.T) *namespaceClient {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := newFilesMetaClient(t, server)
	return &namespaceClient{server: server, client: client, ctx: ctx, session: session, nextID: session.NextMessageID}
}

func (f *namespaceClient) binding() state.Binding {
	return state.Binding{SessionID: f.session.SessionID, TreeID: f.session.TreeID}
}

func (f *namespaceClient) create(t *testing.T, path string, kind smb.Kind) smb.Resolved {
	t.Helper()
	resolved, err := f.server.options.Storage.Lookup(f.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		resolved, err = f.server.options.Storage.Create(f.ctx, resolved.Name, kind)
		if err != nil {
			t.Fatal(err)
		}
	}
	return resolved
}

func (f *namespaceClient) open(t *testing.T, path string, kind smb.Kind, access uint32, sharing state.ShareMode) state.Open {
	t.Helper()
	resolved := f.create(t, path, kind)
	reservation, status := f.server.options.State.Reserve(state.OpenRequest{
		Object: resolved.Object, Binding: f.binding(), User: f.server.options.Account.User, Share: f.server.options.ShareName,
		GrantedAccess: access, Sharing: sharing,
	})
	namespaceStatus(t, status, smb.StatusSuccess)
	handle, err := f.server.options.Storage.Open(f.ctx, resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		namespaceStatus(t, f.server.options.State.Abort(reservation), smb.StatusSuccess)
		t.Fatal(err)
	}
	open, status := f.server.options.State.Commit(reservation, state.Grant{Handle: handle})
	if status != smb.StatusSuccess {
		if err := f.server.options.Storage.Close(f.ctx, handle); err != nil {
			t.Error(err)
		}
		namespaceStatus(t, f.server.options.State.Abort(reservation), smb.StatusSuccess)
		t.Fatal(status)
	}
	return open
}

func namespaceStatus(t *testing.T, got, want smb.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %#x, want %#x", got, want)
	}
}

func (f *namespaceClient) set(t *testing.T, open state.Open, class wire.FileInfoClass, buffer []byte) smb.Status {
	t.Helper()
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{
		InfoType: wire.InfoFile, InfoClass: uint8(class), Input: buffer,
		ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{
		Command: wire.SetInfo, MessageID: f.nextID, SessionID: f.session.SessionID,
		TreeID: f.session.TreeID, CreditCharge: 1, Credit: 1,
	}, Body: body}
	f.nextID++
	response := ioRoundTrip(f.ctx, t, f.client, message)
	if response.Header.Status == smb.StatusSuccess {
		if _, err = wire.DecodeSetInfoResponse(response); err != nil {
			t.Fatal(err)
		}
	}
	return response.Header.Status
}

func (f *namespaceClient) rename(t *testing.T, open state.Open, path string, replace bool) smb.Status {
	t.Helper()
	buffer, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: path, ReplaceIfExists: replace})
	if err != nil {
		t.Fatal(err)
	}
	return f.set(t, open, wire.ClassFileRename, buffer)
}

func (f *namespaceClient) disposition(t *testing.T, open state.Open, pending bool) smb.Status {
	t.Helper()
	buffer, err := wire.EncodeFileDispositionInformation(wire.FileDispositionInformation{DeletePending: pending})
	if err != nil {
		t.Fatal(err)
	}
	return f.set(t, open, wire.ClassFileDisposition, buffer)
}

func (f *namespaceClient) write(t *testing.T, open state.Open, data string) {
	t.Helper()
	if n, err := f.server.options.Storage.WriteAt(f.ctx, open.Handle, []byte(data), 0); err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
}

func (f *namespaceClient) data(t *testing.T, open state.Open, want string) {
	t.Helper()
	buffer := make([]byte, len(want)+1)
	n, err := f.server.options.Storage.ReadAt(f.ctx, open.Handle, buffer, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(buffer[:n]) != want {
		t.Fatalf("data = %q, want %q", buffer[:n], want)
	}
}

func (f *namespaceClient) name(t *testing.T, path string, inode smb.Inode) {
	t.Helper()
	resolved, err := f.server.options.Storage.Lookup(f.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Exists != (inode != 0) || resolved.Object.Inode != inode {
		t.Fatalf("lookup %q = %+v, want inode %d", path, resolved, inode)
	}
}

func (f *namespaceClient) close(t *testing.T, open state.Open) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: wire.FileID(open.ID)})
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(f.ctx, t, f.client, wire.Message{Header: wire.Header{
		Command: wire.Close, MessageID: f.nextID, SessionID: f.session.SessionID,
		TreeID: f.session.TreeID, CreditCharge: 1, Credit: 1,
	}, Body: body})
	f.nextID++
	if len(response) != 1 {
		t.Fatalf("CLOSE replies = %d", len(response))
	}
	namespaceStatus(t, response[0].Header.Status, smb.StatusSuccess)
	if _, err := wire.DecodeCloseResponse(response[0]); err != nil {
		t.Fatal(err)
	}
	_, status := f.server.options.State.Find(open.ID, f.binding())
	namespaceStatus(t, status, smb.StatusFileClosed)
}

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
	namespaceStatus(t, f.disposition(t, stream, true), smb.StatusNotSupported)
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
