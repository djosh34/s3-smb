package server

import (
	"context"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// openAs opens or creates name with the given access, sharing and create
// options, failing the test unless that succeeds.
func openAs(t *testing.T, client *testClient, name string, access, share, options uint32) wire.FileID {
	t.Helper()
	request := wire.CreateRequest{Name: name, DesiredAccess: access, ShareAccess: share, Disposition: fileOpenIf, Options: options}
	result, status := client.create(t, smbtest.CreateOptions{Request: request})
	if status != smb.StatusSuccess {
		t.Fatalf("open %q: status %#x", name, status)
	}
	return result.Reply.ID
}

// openStatus opens the existing name for reading, closes it again and
// returns the CREATE status.
func openStatus(t *testing.T, client *testClient, name string) smb.Status {
	t.Helper()
	request := wire.CreateRequest{Name: name, DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen}
	result, status := client.create(t, smbtest.CreateOptions{Request: request})
	if status == smb.StatusSuccess {
		closeOK(t, client, result.Reply.ID)
	}
	return status
}

func closeOK(t *testing.T, client *testClient, id wire.FileID) {
	t.Helper()
	if status := client.close(t, id); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
}

// seed creates name holding data.
func seed(t *testing.T, client *testClient, name, data string) {
	t.Helper()
	id := client.open(t, name)
	if status := client.write(t, wire.WriteRequest{ID: id, Data: []byte(data)}); status != smb.StatusSuccess {
		t.Fatalf("WRITE %q: status %#x", name, status)
	}
	closeOK(t, client, id)
}

// content reads the data stored under name straight from the engine.
func (s *testServer) content(t *testing.T, name string) string {
	t.Helper()
	object := s.object(t, name)
	attr, err := s.storage.GetAttr(t.Context(), object)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := s.storage.Open(t.Context(), object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, attr.Size)
	n, err := s.storage.ReadAt(t.Context(), handle, data, 0)
	if closeErr := s.storage.Close(t.Context(), handle); err != nil || closeErr != nil {
		t.Fatalf("read %q: %v, %v", name, err, closeErr)
	}
	return string(data[:n])
}

// expectContent checks the data of every name in want. An empty string
// means the name must not exist.
func (s *testServer) expectContent(t *testing.T, want map[string]string) {
	t.Helper()
	for name, data := range want {
		if data == "" {
			if s.exists(t, name) {
				t.Errorf("%q still exists", name)
			}
		} else if got := s.content(t, name); got != data {
			t.Errorf("%q holds %q, want %q", name, got, data)
		}
	}
}

func setFileInfo[T any](t *testing.T, client *testClient, id wire.FileID, class wire.FileInfoClass, encoder func(T) ([]byte, error), value T) smb.Status {
	t.Helper()
	return client.setInfo(t, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), Input: encode(t, encoder, value)})
}

func setDeletePending(t *testing.T, client *testClient, id wire.FileID, pending bool) smb.Status {
	t.Helper()
	return setFileInfo(t, client, id, wire.ClassFileDisposition, wire.EncodeFileDispositionInformation, wire.FileDispositionInformation{DeletePending: pending})
}

func rename(t *testing.T, client *testClient, id wire.FileID, name string, replace bool) smb.Status {
	t.Helper()
	return setFileInfo(t, client, id, wire.ClassFileRename, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: name, ReplaceIfExists: replace})
}

func TestDeleteOnCloseNeedsDeleteAccess(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "existing", "keep")
	for _, test := range []struct {
		name        string
		disposition uint32
	}{{"missing", fileCreateDisposition}, {"existing", fileOverwrite}} {
		request := wire.CreateRequest{Name: test.name, DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: test.disposition, Options: fileDeleteOnClose}
		if _, status := client.create(t, smbtest.CreateOptions{Request: request}); status != smb.StatusAccessDenied {
			t.Fatalf("%s: status %#x", test.name, status)
		}
	}
	srv.expectContent(t, map[string]string{"missing": "", "existing": "keep"})
	// GENERIC_ALL grants DELETE once the server expands it.
	closeOK(t, client, openAs(t, client, "missing", genericAll, 7, fileDeleteOnClose))
	srv.expectContent(t, map[string]string{"missing": ""})
}

// However the last open ends, delete on close removes exactly its own file
// and leaves other files.
func TestDeleteOnCloseRemovesOnlyItsObject(t *testing.T) {
	for _, ending := range []string{"close", "drop", "logoff", "tree disconnect"} {
		t.Run(ending, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			seed(t, client, "data", "data")
			seed(t, client, "unrelated", "unrelated")
			id := openAs(t, client, "data", fileDelete, 7, fileDeleteOnClose)
			switch ending {
			case "close":
				closeOK(t, client, id)
			case "drop":
				client.drop(t)
			case "logoff":
				if status := client.call(t, wire.Logoff, encode(t, wire.EncodeLogoffRequest, wire.EmptyRequest{}), 1).Header.Status; status != smb.StatusSuccess {
					t.Fatalf("LOGOFF status %#x", status)
				}
			case "tree disconnect":
				if status := client.call(t, wire.TreeDisconnect, encode(t, wire.EncodeTreeDisconnectRequest, wire.EmptyRequest{}), 1).Header.Status; status != smb.StatusSuccess {
					t.Fatalf("TREE_DISCONNECT status %#x", status)
				}
			}
			srv.expectContent(t, map[string]string{"data": "", "unrelated": "unrelated"})
		})
	}
}

// A pending delete refuses new opens of the file and happens when the last
// open closes, even if that open is on another connection.
func TestDeletePendingWaitsForLastOpen(t *testing.T) {
	srv := newTestServer(t)
	client, other := srv.connect(t), srv.connect(t)
	seed(t, client, "data", "base")
	held := openAs(t, other, "data", fileReadData, 7, 0)
	id := openAs(t, client, "data", fileDelete, 7, 0)
	if status := setDeletePending(t, client, id, true); status != smb.StatusSuccess {
		t.Fatalf("SET_INFO status %#x", status)
	}
	client.drop(t)
	if status := openStatus(t, other, "data"); status != smb.StatusDeletePending {
		t.Fatalf("open: status %#x", status)
	}
	srv.expectContent(t, map[string]string{"data": "base"})
	closeOK(t, other, held)
	srv.expectContent(t, map[string]string{"data": ""})
}

// Each open sets and clears only its own delete intent. Delete on close from
// CREATE stays set when the disposition is cleared.
func TestDeletePendingIsPerOpen(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "data", "base")
	first := openAs(t, client, "data", fileDelete, 7, 0)
	second := openAs(t, client, "data", fileDelete, 7, 0)
	for _, step := range []struct {
		id      wire.FileID
		pending bool
		want    smb.Status
	}{
		{first, true, smb.StatusDeletePending},
		{second, true, smb.StatusDeletePending},
		{first, false, smb.StatusDeletePending},
		{second, false, smb.StatusSuccess},
	} {
		if status := setDeletePending(t, client, step.id, step.pending); status != smb.StatusSuccess {
			t.Fatalf("SET_INFO status %#x", status)
		}
		if status := openStatus(t, client, "data"); status != step.want {
			t.Fatalf("open after %+v: status %#x", step, status)
		}
	}
	closeOK(t, client, first)
	closeOK(t, client, second)

	id := openAs(t, client, "data", fileDelete, 7, fileDeleteOnClose)
	for _, pending := range []bool{true, false} {
		if status := setDeletePending(t, client, id, pending); status != smb.StatusSuccess {
			t.Fatalf("SET_INFO status %#x", status)
		}
	}
	closeOK(t, client, id)
	srv.expectContent(t, map[string]string{"data": ""})
}

func TestDeletePendingNeedsDeleteAccess(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "data", "base")
	id := openAs(t, client, "data", fileReadData|fileWriteData, 7, 0)
	for _, pending := range []bool{true, false} {
		if status := setDeletePending(t, client, id, pending); status != smb.StatusAccessDenied {
			t.Fatalf("SET_INFO %t: status %#x", pending, status)
		}
	}
	if status := openStatus(t, client, "data"); status != smb.StatusSuccess {
		t.Fatalf("open after refused delete: status %#x", status)
	}
}

func TestDeleteDirectory(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	directory := openAs(t, client, "directory", fileDelete, 7, fileDirectoryFile)
	seed(t, client, "directory/child", "child")
	if status := setDeletePending(t, client, directory, true); status != smb.StatusDirectoryNotEmpty {
		t.Fatalf("non-empty directory: status %#x", status)
	}
	child := openAs(t, client, "directory/child", fileDelete, 7, fileDeleteOnClose)
	closeOK(t, client, child)
	srv.expectContent(t, map[string]string{"directory/child": ""})
	if status := setDeletePending(t, client, directory, true); status != smb.StatusSuccess {
		t.Fatalf("empty directory: status %#x", status)
	}
	closeOK(t, client, directory)
	srv.expectContent(t, map[string]string{"directory": ""})
}

// A pending delete follows its file through a rename and leaves whatever
// takes the old name alone.
func TestDeleteFollowsRename(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "data", "old")
	id := client.open(t, "data")
	if status := setDeletePending(t, client, id, true); status != smb.StatusSuccess {
		t.Fatalf("SET_INFO status %#x", status)
	}
	if status := rename(t, client, id, "renamed", false); status != smb.StatusSuccess {
		t.Fatalf("rename status %#x", status)
	}
	seed(t, client, "data", "replacement")
	closeOK(t, client, id)
	srv.expectContent(t, map[string]string{"renamed": "", "data": "replacement"})
}

// While storage is still deleting a closed file, new opens see the delete
// pending; once it is gone the name is free for a new file.
func TestDeleteInProgressRefusesOpens(t *testing.T) {
	srv := newTestServer(t)
	client, other := srv.connect(t), srv.connect(t)
	seed(t, client, "data", "old")
	id := openAs(t, client, "data", fileDelete, 7, fileDeleteOnClose)
	entered, blocked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	t.Cleanup(release)
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Close = func(ctx context.Context, handle smb.Handle) error {
			close(entered)
			<-blocked
			return srv.storage.Close(ctx, handle)
		}
	})
	request := client.send(t, wire.Close, encode(t, wire.EncodeCloseRequest, wire.CloseRequest{ID: id}), 1)
	<-entered
	srv.faults.set(func(hooks *storageHooks) { hooks.Close = nil })
	if status := openStatus(t, other, "data"); status != smb.StatusDeletePending {
		t.Fatalf("open during deletion: status %#x", status)
	}
	release()
	if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
	seed(t, other, "data", "new")
	srv.expectContent(t, map[string]string{"data": "new"})
}

// Delete on close removes the inode it opened. If another file has taken
// the name by the time storage removes it, that file stays.
func TestDeleteOnCloseLeavesReplacementAlone(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "data", "old")
	seed(t, client, "replacement", "new")
	id := openAs(t, client, "data", fileDelete, 7, fileDeleteOnClose)
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Remove = func(ctx context.Context, name smb.Name, expect smb.Inode) error {
			replacement, err := srv.storage.Lookup(ctx, "replacement")
			if err != nil {
				return err
			}
			if err = srv.storage.Rename(ctx, smb.RenameRequest{Source: replacement.Name, SourceInode: replacement.Object, Destination: name, DestinationInode: expect, Replace: true}); err != nil {
				return err
			}
			return srv.storage.Remove(ctx, name, expect)
		}
	})
	if status := client.close(t, id); status != smb.StatusObjectNameNotFound {
		t.Fatalf("CLOSE status %#x", status)
	}
	srv.expectContent(t, map[string]string{"data": "new"})
}

// A CLOSE that fails in storage still ends the open, releases its sharing
// and runs its delete on close.
func TestFailedCloseStillCloses(t *testing.T) {
	for _, failure := range []string{"path", "post-query"} {
		t.Run(failure, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			id := openAs(t, client, "data", fileAllAccess, 0, fileDeleteOnClose)
			srv.faults.set(func(hooks *storageHooks) {
				if failure == "path" {
					hooks.PathOf = func(context.Context, smb.Inode) (string, error) { return "", smb.ErrIO }
				} else {
					hooks.GetAttr = func(context.Context, smb.Inode) (smb.Attr, error) { return smb.Attr{}, smb.ErrIO }
				}
			})
			request := wire.CloseRequest{ID: id, Flags: 1}
			if status := client.call(t, wire.Close, encode(t, wire.EncodeCloseRequest, request), 1).Header.Status; status != smb.StatusIODeviceError {
				t.Fatalf("CLOSE status %#x", status)
			}
			srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
			if status := client.close(t, id); status != smb.StatusFileClosed {
				t.Fatalf("second CLOSE status %#x", status)
			}
			if failure == "post-query" {
				srv.expectContent(t, map[string]string{"data": ""})
			} else {
				closeOK(t, client, openAs(t, client, "data", fileAllAccess, 0, 0))
			}
		})
	}
}

// Renaming moves the open file, which every open of it then sees under the
// new name, and changes no other name.
func TestRenameMovesTheOpenFile(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	openAs(t, client, "left", fileAllAccess, 7, fileDirectoryFile)
	openAs(t, client, "right", fileAllAccess, 7, fileDirectoryFile)
	seed(t, client, "left/other", "left")
	seed(t, client, "right/other", "right")
	source := client.open(t, "left/source")
	second := client.open(t, "left/source")
	if status := client.write(t, wire.WriteRequest{ID: source, Data: []byte("before")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	// A leading separator names the destination from the share root.
	if status := rename(t, client, source, "\\right\\renamed", false); status != smb.StatusSuccess {
		t.Fatalf("rename status %#x", status)
	}
	if status := client.write(t, wire.WriteRequest{ID: source, Data: []byte("after!")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	if data, status := client.read(t, wire.ReadRequest{ID: second, Length: 64}); status != smb.StatusSuccess || string(data) != "after!" {
		t.Fatalf("READ through the second open = %q, %#x", data, status)
	}
	srv.expectContent(t, map[string]string{"left/source": "", "right/renamed": "after!", "left/other": "left", "right/other": "right"})
}

func TestRenameOntoExistingName(t *testing.T) {
	for _, test := range []struct {
		name, held string
		want       smb.Status
		replace    bool
	}{
		{"collision", "", smb.StatusObjectNameCollision, false},
		{"replace", "", smb.StatusSuccess, true},
		{"replace an open file", "destination", smb.StatusAccessDenied, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			seed(t, client, "source", "source")
			seed(t, client, "destination", "destination")
			if test.held != "" {
				client.open(t, test.held)
			}
			id := client.open(t, "source")
			if status := rename(t, client, id, "destination", test.replace); status != test.want {
				t.Fatalf("rename status %#x", status)
			}
			want := map[string]string{"source": "source", "destination": "destination"}
			if test.want == smb.StatusSuccess {
				want = map[string]string{"source": "", "destination": "source"}
			}
			srv.expectContent(t, want)
		})
	}
}

func TestRenameRefusals(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "source", "source")
	id := client.open(t, "source")
	noDelete := openAs(t, client, "source", fileReadData|fileWriteData, 7, 0)
	input := func(name string, root uint64) []byte {
		return encode(t, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: name, RootDirectory: root})
	}
	for _, test := range []struct {
		name  string
		input []byte
		id    wire.FileID
		want  smb.Status
	}{
		{"short buffer", []byte{1}, id, smb.StatusInvalidParameter},
		{"root directory handle", input("destination", 1), id, smb.StatusNotSupported},
		{"to a stream name", input("source:new", 0), id, smb.StatusNotSupported},
		{"no delete access", input("destination", 0), noDelete, smb.StatusAccessDenied},
		{"parent component", input("../destination", 0), id, smb.StatusObjectNameInvalid},
		{"missing parent", input("missing/destination", 0), id, smb.StatusObjectPathNotFound},
	} {
		if status := client.setInfo(t, wire.SetInfoRequest{ID: test.id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileRename), Input: test.input}); status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	srv.expectContent(t, map[string]string{"source": "source", "destination": ""})
}

// When the source moves between the server finding it and locking its
// directory, the rename starts again from the source's new place.
func TestRenameRetriesWhenSourceMoves(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	openAs(t, client, "left", fileAllAccess, 7, fileDirectoryFile)
	openAs(t, client, "right", fileAllAccess, 7, fileDirectoryFile)
	seed(t, client, "left/source", "source")
	id := client.open(t, "left/source")
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Lookup = func(ctx context.Context, path string) (smb.Resolved, error) {
			resolved, err := srv.storage.Lookup(ctx, path)
			if err != nil || path != "left/source" {
				return resolved, err
			}
			srv.faults.set(func(hooks *storageHooks) { hooks.Lookup = nil })
			moved, err := srv.storage.Lookup(ctx, "right/moved")
			if err != nil {
				return smb.Resolved{}, err
			}
			return resolved, srv.storage.Rename(ctx, smb.RenameRequest{Source: resolved.Name, SourceInode: resolved.Object, Destination: moved.Name})
		}
	})
	if status := rename(t, client, id, "right/final", false); status != smb.StatusSuccess {
		t.Fatalf("rename status %#x", status)
	}
	srv.expectContent(t, map[string]string{"left/source": "", "right/moved": "", "right/final": "source"})
}

// A rename or delete waiting for another rename in the same directory can be
// cancelled, and then changes nothing.
func TestNamespaceWaitCanBeCancelled(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	openAs(t, client, "left", fileAllAccess, 7, fileDirectoryFile)
	openAs(t, client, "right", fileAllAccess, 7, fileDirectoryFile)
	seed(t, client, "left/moving", "moving")
	seed(t, client, "left/waiting", "waiting")
	moving, waiting := client.open(t, "left/moving"), client.open(t, "left/waiting")
	entered, blocked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	t.Cleanup(release)
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Rename = func(ctx context.Context, request smb.RenameRequest) error {
			close(entered)
			<-blocked
			return srv.storage.Rename(ctx, request)
		}
	})
	setInfoBody := func(class wire.FileInfoClass, id wire.FileID, input []byte) []byte {
		return encode(t, wire.EncodeSetInfoRequest, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input})
	}
	first := client.send(t, wire.SetInfo, setInfoBody(wire.ClassFileRename, moving, encode(t, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: "right/moved"})), 1)
	<-entered
	client.interim(t, first)
	for _, input := range []struct {
		data  []byte
		class wire.FileInfoClass
	}{
		{encode(t, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: "right/other"}), wire.ClassFileRename},
		{encode(t, wire.EncodeFileDispositionInformation, wire.FileDispositionInformation{DeletePending: true}), wire.ClassFileDisposition},
	} {
		request := client.send(t, wire.SetInfo, setInfoBody(input.class, waiting, input.data), 1)
		client.cancelAsync(t, client.interim(t, request))
		if status := client.receive(t, request).Header.Status; status != smb.StatusCancelled {
			t.Fatalf("cancelled SET_INFO status %#x", status)
		}
	}
	release()
	if status := client.receive(t, first).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("rename status %#x", status)
	}
	if status := openStatus(t, client, "left/waiting"); status != smb.StatusSuccess {
		t.Fatalf("open after cancelled delete: status %#x", status)
	}
	srv.expectContent(t, map[string]string{"right/moved": "moving", "left/waiting": "waiting", "right/other": ""})
}
