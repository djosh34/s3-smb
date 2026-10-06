package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// fullAccess asks for GENERIC_ALL on name, sharing everything.
func fullAccess(name string, disposition uint32) wire.CreateRequest {
	return wire.CreateRequest{Name: name, DesiredAccess: genericAll, ShareAccess: 7, Disposition: disposition}
}

// createAs sends request and returns the decoded reply with its status.
func createAs(t *testing.T, client *testClient, request wire.CreateRequest) (wire.CreateResponse, smb.Status) {
	t.Helper()
	result, status := client.create(t, smbtest.CreateOptions{Request: request})
	return result.Reply, status
}

// mustOpen sends request and fails the test unless it succeeds.
func mustOpen(t *testing.T, client *testClient, request wire.CreateRequest) wire.CreateResponse {
	t.Helper()
	reply, status := createAs(t, client, request)
	if status != smb.StatusSuccess {
		t.Fatalf("CREATE %q disposition %d: status %#x", request.Name, request.Disposition, status)
	}
	return reply
}

// seedWithAttributes creates name with attributes, holding data.
func seedWithAttributes(t *testing.T, client *testClient, name, data string, attributes uint32) {
	t.Helper()
	request := fullAccess(name, fileCreateDisposition)
	request.FileAttributes = attributes
	id := mustOpen(t, client, request).ID
	writeFile(t, client, id, []byte(data))
	closeOK(t, client, id)
}

func TestCreateDispositions(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	for _, test := range []struct {
		disposition       uint32
		want              smb.Status
		action            uint32
		exists, keepsData bool
	}{
		{fileSupersede, smb.StatusSuccess, 2, false, false},
		{fileSupersede, smb.StatusSuccess, 0, true, false},
		{fileOpen, smb.StatusObjectNameNotFound, 0, false, false},
		{fileOpen, smb.StatusSuccess, 1, true, true},
		{fileCreateDisposition, smb.StatusSuccess, 2, false, false},
		{fileCreateDisposition, smb.StatusObjectNameCollision, 0, true, true},
		{fileOpenIf, smb.StatusSuccess, 2, false, false},
		{fileOpenIf, smb.StatusSuccess, 1, true, true},
		{fileOverwrite, smb.StatusObjectNameNotFound, 0, false, false},
		{fileOverwrite, smb.StatusSuccess, 3, true, false},
		{fileOverwriteIf, smb.StatusSuccess, 2, false, false},
		{fileOverwriteIf, smb.StatusSuccess, 3, true, false},
	} {
		name := fmt.Sprintf("disposition %d exists %t", test.disposition, test.exists)
		if test.exists {
			seed(t, client, name, "old")
		}
		content := ""
		if test.keepsData {
			content = "old"
		}
		reply, status := createAs(t, client, fullAccess(name, test.disposition))
		if status != test.want {
			t.Errorf("%s: status %#x, want %#x", name, status, test.want)
			continue
		}
		if status == smb.StatusSuccess {
			if reply.Action != test.action || reply.Size != uint64(len(content)) || reply.OplockLevel != 0 {
				t.Errorf("%s: reply %+v, want action %d size %d", name, reply, test.action, len(content))
			}
			closeOK(t, client, reply.ID)
		}
		if status == smb.StatusObjectNameNotFound {
			if srv.exists(t, name) {
				t.Errorf("%s: CREATE made the file", name)
			}
		} else if got := srv.content(t, name); got != content {
			t.Errorf("%s: file holds %q, want %q", name, got, content)
		}
	}
}

func TestCreateExpandsGenericAccess(t *testing.T) {
	client := newTestServer(t).connect(t)
	for _, test := range []struct {
		name          string
		desired, want uint32
	}{
		{"generic read", genericRead, 0x00120089},
		{"generic write", genericWrite, 0x00120116},
		{"generic execute", genericExecute, 0x001200a0},
		{"execute", fileExecute, fileExecute},
		{"generic all", genericAll, fileAllAccess},
		{"maximum allowed", maximumAllowed, fileAllAccess},
		{"specific", fileAppendData, fileAppendData},
		{"combined", genericRead | genericWrite | fileDelete, 0x0013019f},
	} {
		id := mustOpen(t, client, wire.CreateRequest{Name: test.name, DesiredAccess: test.desired, ShareAccess: 7, Disposition: fileOpenIf}).ID
		expectInfo(t, client, id, wire.ClassFileAccess, wire.DecodeFileAccessInformation, wire.FileAccessInformation{Access: test.want})
	}
}

// Supersede needs DELETE, overwrite needs WRITE_DATA, and replacing a hidden
// or system file needs those attributes in the request. A refusal changes
// neither data nor attributes.
func TestCreateReplacementNeedsAccess(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	for _, test := range []struct {
		disposition, access, existing, requested uint32
		want                                     smb.Status
	}{
		{fileSupersede, fileReadData | fileWriteData, 0, 0, smb.StatusAccessDenied},
		{fileOverwrite, fileReadData | fileDelete, 0, 0, smb.StatusAccessDenied},
		{fileOverwriteIf, fileAppendData, 0, 0, smb.StatusAccessDenied},
		{fileSupersede, fileDelete, 0, 0, smb.StatusSuccess},
		{fileOverwrite, fileWriteData, 0, 0, smb.StatusSuccess},
		{fileOverwrite, genericAll, 0x2, 0, smb.StatusAccessDenied},
		{fileSupersede, genericAll, 0x6, 0x2, smb.StatusAccessDenied},
		{fileOverwriteIf, genericAll, 0x6, 0x4, smb.StatusAccessDenied},
		{fileOverwrite, genericAll, 0x2, 0x2, smb.StatusSuccess},
		{fileSupersede, genericAll, 0x6, 0x6, smb.StatusSuccess},
	} {
		name := fmt.Sprintf("%d %#x %#x %#x", test.disposition, test.access, test.existing, test.requested)
		seedWithAttributes(t, client, name, "keep", test.existing)
		request := wire.CreateRequest{Name: name, DesiredAccess: test.access, ShareAccess: 7, Disposition: test.disposition, FileAttributes: test.requested}
		reply, status := createAs(t, client, request)
		if status != test.want {
			t.Errorf("%s: status %#x, want %#x", name, status, test.want)
			continue
		}
		want := "keep"
		if status == smb.StatusSuccess {
			want = ""
			closeOK(t, client, reply.ID)
		} else if got := srv.attributes(t, srv.object(t, name)); got != test.existing|0x20 {
			t.Errorf("%s: refusal changed the attributes", name)
		}
		if got := srv.content(t, name); got != want {
			t.Errorf("%s: file holds %q, want %q", name, got, want)
		}
	}
}

func (s *testServer) attributes(t *testing.T, object smb.Inode) uint32 {
	t.Helper()
	attr, err := s.storage.GetAttr(t.Context(), object)
	if err != nil {
		t.Fatal(err)
	}
	return attr.Attributes
}

// New files get ARCHIVE and drop NORMAL and the attributes the server does
// not support: sparse, reparse point, compressed and encrypted. Overwrite
// adds the requested attributes to the existing ones; supersede replaces them.
func TestCreateAttributes(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	const unsupported = 0x200 | 0x400 | 0x800 | 0x4000
	for _, test := range []struct {
		requested, options, want uint32
	}{
		{0, 0, 0x20},
		{0x80, 0, 0x20},
		{0x2, 0, 0x22},
		{0x100, 0, 0x120},
		{0x2082, 0, 0x2022},
		{unsupported | 0x3127, 0, 0x3127},
		{unsupported, fileDirectoryFile, 0x10},
		{0x3027, fileDirectoryFile, 0x3037},
	} {
		name := fmt.Sprintf("new %#x %#x", test.requested, test.options)
		request := fullAccess(name, fileCreateDisposition)
		request.FileAttributes, request.Options = test.requested, test.options
		reply := mustOpen(t, client, request)
		closeOK(t, client, reply.ID)
		if got := srv.attributes(t, srv.object(t, name)); reply.Attributes != test.want || got != test.want {
			t.Errorf("%s: reply %#x, stored %#x, want %#x", name, reply.Attributes, got, test.want)
		}
	}
	for _, test := range []struct {
		disposition, requested, want uint32
	}{
		{fileOverwrite, 0, 0x120},
		{fileOverwrite, 0x80, 0x120},
		{fileOverwriteIf, 0x2, 0x122},
		{fileOverwrite, 0x202, 0x122},
		{fileSupersede, 0x202, 0x22},
		{fileSupersede, 0, 0x20},
	} {
		name := fmt.Sprintf("replaced %d %#x", test.disposition, test.requested)
		seedWithAttributes(t, client, name, "old", 0x100)
		request := fullAccess(name, test.disposition)
		request.FileAttributes = test.requested
		reply := mustOpen(t, client, request)
		closeOK(t, client, reply.ID)
		if got := srv.attributes(t, srv.object(t, name)); reply.Attributes != test.want || got != test.want {
			t.Errorf("%s: reply %#x, stored %#x, want %#x", name, reply.Attributes, got, test.want)
		}
	}
}

func TestCreateDirectoryOptions(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "file", "keep")
	closeOK(t, client, openDirectory(t, client, "dir"))
	for _, test := range []struct {
		name                 string
		options, disposition uint32
		want                 smb.Status
	}{
		{"file", fileDirectoryFile, fileOpen, smb.StatusNotADirectory},
		{"file", fileDirectoryFile, fileOpenIf, smb.StatusNotADirectory},
		{"file", fileDirectoryFile, fileCreateDisposition, smb.StatusObjectNameCollision},
		{"file", fileDirectoryFile | fileNonDirectoryFile, fileOpen, smb.StatusInvalidParameter},
		{"dir", fileNonDirectoryFile, fileOpen, smb.StatusFileIsADirectory},
		{"dir", 0, fileOverwrite, smb.StatusFileIsADirectory},
		{"dir", fileDirectoryFile, fileSupersede, smb.StatusInvalidParameter},
		{"new", fileDirectoryFile, fileOverwrite, smb.StatusInvalidParameter},
		{"new", fileDirectoryFile, fileOverwriteIf, smb.StatusInvalidParameter},
	} {
		request := fullAccess(test.name, test.disposition)
		request.Options = test.options
		if _, status := createAs(t, client, request); status != test.want {
			t.Errorf("%q options %#x disposition %d: status %#x, want %#x", test.name, test.options, test.disposition, status, test.want)
		}
	}
	srv.expectContent(t, map[string]string{"file": "keep", "new": ""})
	if got := srv.attributes(t, srv.object(t, "dir")); got != 0x10 {
		t.Errorf("directory attributes %#x", got)
	}
}

// Refused requests change nothing, whatever their disposition, and the
// connection goes on.
func TestCreateRefusals(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "file", "keep")
	for name, test := range map[string]struct {
		change func(*wire.CreateRequest)
		want   smb.Status
	}{
		"backslash first":       {func(r *wire.CreateRequest) { r.Name = "\\" + r.Name }, smb.StatusInvalidParameter},
		"slash first":           {func(r *wire.CreateRequest) { r.Name = "/" + r.Name }, smb.StatusInvalidParameter},
		"two backslashes first": {func(r *wire.CreateRequest) { r.Name = "\\\\" + r.Name }, smb.StatusInvalidParameter},
		"impersonation 4":       {func(r *wire.CreateRequest) { r.ImpersonationLevel = 4 }, smb.StatusBadImpersonationLevel},
		"impersonation max":     {func(r *wire.CreateRequest) { r.ImpersonationLevel = math.MaxUint32 }, smb.StatusBadImpersonationLevel},
		"open by file ID":       {func(r *wire.CreateRequest) { r.Options = fileOpenByFileID }, smb.StatusNotSupported},
		"reserve opfilter":      {func(r *wire.CreateRequest) { r.Options, r.ShareAccess = fileReserveOpfilter, 0 }, smb.StatusNotSupported},
		"unknown disposition":   {func(r *wire.CreateRequest) { r.Disposition = 6 }, smb.StatusInvalidParameter},
		"unknown share bit":     {func(r *wire.CreateRequest) { r.ShareAccess = 8 }, smb.StatusInvalidParameter},
	} {
		for _, request := range []wire.CreateRequest{fullAccess("new", fileOpenIf), fullAccess("file", fileSupersede)} {
			test.change(&request)
			if _, status := createAs(t, client, request); status != test.want {
				t.Errorf("%s on %q: status %#x, want %#x", name, request.Name, status, test.want)
			}
		}
	}
	srv.expectContent(t, map[string]string{"file": "keep", "new": ""})
	// Impersonation levels up to delegation are fine, and nothing was left
	// reserved: an open that shares nothing succeeds.
	request := wire.CreateRequest{Name: "file", DesiredAccess: genericAll, Disposition: fileOpen, ImpersonationLevel: 3}
	closeOK(t, client, mustOpen(t, client, request).ID)
}

// macOS asks for the on-disk file ID with a QFid context. The server has no
// stable file IDs to give, so it answers the CREATE without one.
func TestCreateIgnoresFileIDQuery(t *testing.T) {
	client := newTestServer(t).connect(t)
	query := wire.CreateContext{Name: "QFid"}
	for _, request := range []wire.CreateRequest{fullAccess("file", fileOpenIf), {DesiredAccess: fileGenericRead, ShareAccess: 7, Disposition: fileOpen, Options: fileDirectoryFile}} {
		request.Contexts = []wire.CreateContext{query}
		if reply := mustOpen(t, client, request); len(reply.Contexts) != 0 {
			t.Errorf("CREATE %q answered contexts %+v", request.Name, reply.Contexts)
		}
	}
}

// countHandles counts storage opens and closes, to check that every failed
// CREATE gives its storage handle back.
func countHandles(srv *testServer) (opened, closed *atomic.Int32) {
	opened, closed = new(atomic.Int32), new(atomic.Int32)
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Open = func(ctx context.Context, object smb.Inode, access smb.Access) (smb.Handle, error) {
			opened.Add(1)
			return srv.storage.Open(ctx, object, access)
		}
		hooks.Close = func(ctx context.Context, handle smb.Handle) error {
			closed.Add(1)
			return srv.storage.Close(ctx, handle)
		}
	})
	return opened, closed
}

func TestCreateFailureReleasesEverything(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "file", "keep")
	opened, closed := countHandles(srv)
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Truncate = func(context.Context, smb.Handle, uint64) error { return smb.ErrIO }
	})
	request := fullAccess("file", fileSupersede)
	request.ShareAccess = 0
	if _, status := createAs(t, client, request); status != smb.StatusIODeviceError {
		t.Fatalf("CREATE with a failing truncate: status %#x", status)
	}
	// The share root has no name to delete, so the open table refuses it.
	root := wire.CreateRequest{DesiredAccess: genericAll, Disposition: fileOpen, Options: fileDeleteOnClose}
	if _, status := createAs(t, client, root); status != smb.StatusInvalidParameter {
		t.Fatalf("delete on close of the root: status %#x", status)
	}
	if opened.Load() != 2 || closed.Load() != 2 {
		t.Fatalf("%d storage handles opened, %d closed; want 2", opened.Load(), closed.Load())
	}
	srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
	srv.expectContent(t, map[string]string{"file": "keep"})
	request.Disposition = fileOpen
	closeOK(t, client, mustOpen(t, client, request).ID)
	root.Options = 0
	closeOK(t, client, mustOpen(t, client, root).ID)
}

func TestClose(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	base, data := client.open(t, "file"), client.open(t, "data")
	writeFile(t, client, data, []byte("data"))
	request := wire.CloseRequest{ID: data, Flags: 1}
	response, status := decodeReply(t, client.call(t, wire.Close, encode(t, wire.EncodeCloseRequest, request), 1), wire.DecodeCloseResponse)
	if status != smb.StatusSuccess || response.Flags != 1 || response.Size != 4 || response.Attributes != 0x20 {
		t.Fatalf("CLOSE with attributes = %+v, %#x", response, status)
	}
	request = wire.CloseRequest{ID: base}
	response, status = decodeReply(t, client.call(t, wire.Close, encode(t, wire.EncodeCloseRequest, request), 1), wire.DecodeCloseResponse)
	if status != smb.StatusSuccess || response != (wire.CloseResponse{}) {
		t.Fatalf("CLOSE without attributes = %+v, %#x", response, status)
	}
	if status := client.close(t, base); status != smb.StatusFileClosed {
		t.Fatalf("second CLOSE: status %#x", status)
	}
}

// A CLOSE whose storage work fails still ends the open and its share mode.
func TestCloseFailureEndsTheOpen(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	for name, test := range map[string]struct {
		set   func(*storageHooks)
		flags uint16
	}{
		"close": {func(hooks *storageHooks) {
			hooks.Close = func(ctx context.Context, handle smb.Handle) error {
				return errors.Join(smb.ErrIO, srv.storage.Close(ctx, handle))
			}
		}, 0},
		"attributes": {func(hooks *storageHooks) {
			hooks.GetAttr = func(context.Context, smb.Inode) (smb.Attr, error) { return smb.Attr{}, smb.ErrIO }
		}, 1},
	} {
		id := openAs(t, client, name, fileAllAccess, 0, 0)
		srv.faults.set(test.set)
		request := wire.CloseRequest{ID: id, Flags: test.flags}
		if status := client.call(t, wire.Close, encode(t, wire.EncodeCloseRequest, request), 1).Header.Status; status != smb.StatusIODeviceError {
			t.Errorf("%s: CLOSE status %#x", name, status)
		}
		srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
		if status := client.close(t, id); status != smb.StatusFileClosed {
			t.Errorf("%s: second CLOSE status %#x", name, status)
		}
		closeOK(t, client, openAs(t, client, name, fileAllAccess, 0, 0))
	}
}

// Storage that keeps saying a file moved makes CLOSE of a delete-on-close
// open fail after a few tries instead of looking for the name forever.
func TestCloseGivesUpOnAMovingName(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := openAs(t, client, "file", fileAllAccess, 7, fileDeleteOnClose)
	var calls atomic.Int32
	srv.faults.set(func(hooks *storageHooks) {
		hooks.PathOf = func(context.Context, smb.Inode) (string, error) {
			calls.Add(1)
			return "", smb.ErrIdentityChanged
		}
	})
	if status := client.close(t, id); status == smb.StatusSuccess {
		t.Fatal("CLOSE succeeded without finding the name")
	}
	srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
	if calls.Load() == 0 {
		t.Fatal("CLOSE never looked for the name")
	}
	if status := client.close(t, id); status != smb.StatusFileClosed {
		t.Fatalf("second CLOSE: status %#x", status)
	}
	if status := openStatus(t, client, "file"); status != smb.StatusSuccess {
		t.Fatalf("open after the failed delete: status %#x", status)
	}
}
