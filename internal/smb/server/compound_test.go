package server

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func createMessage(t *testing.T, client *testClient, name string, disposition uint32) wire.Message {
	t.Helper()
	request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: disposition}
	return message(t, client, wire.Create, wire.EncodeCreateRequest, request)
}

func closeMessage(t *testing.T, client *testClient, id wire.FileID) wire.Message {
	t.Helper()
	return message(t, client, wire.Close, wire.EncodeCloseRequest, wire.CloseRequest{ID: id})
}

// Related members use the file the CREATE before them opened, also when one
// of them waits on storage and the next has to wait for it.
func TestCompoundRelatedFileID(t *testing.T) {
	for _, held := range []bool{false, true} {
		srv := newTestServer(t)
		client := srv.connect(t)
		messages := []wire.Message{
			createMessage(t, client, "file", fileOpenIf),
			related(message(t, client, wire.Write, wire.EncodeWriteRequest, wire.WriteRequest{ID: placeholder, Data: []byte("data")})),
			related(closeMessage(t, client, placeholder)),
		}
		var statuses []smb.Status
		if held {
			entered, release := srv.holdWrites()
			if err := client.raw.Send(t.Context(), messages); err != nil {
				t.Fatal(err)
			}
			<-entered
			close(release)
			statuses = finalStatuses(t, client, messages)
		} else {
			statuses = sendCompound(t, client, messages...)
		}
		if want := []smb.Status{smb.StatusSuccess, smb.StatusSuccess, smb.StatusSuccess}; !slices.Equal(statuses, want) {
			t.Fatalf("statuses %#x", statuses)
		}
		srv.expectContent(t, map[string]string{"file": "data"})
		client.noExtraReplies(t)
	}
}

// Members of a compound run in order, related or not: once one goes async,
// the rest wait for it. A READ after a WRITE that waits on storage reads the
// written data, and a CLOSE after a WRITE cannot close the file before the
// WRITE has started.
func TestCompoundMembersRunInOrder(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := client.open(t, "file")
	entered, release := srv.holdWrites()
	data := []byte("written before the READ")
	messages := []wire.Message{
		message(t, client, wire.Write, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: data}),
		message(t, client, wire.Read, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Length: 64}),
	}
	if err := client.raw.Send(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	<-entered
	client.interim(t, messages[0].Header)
	// Out of order, the READ would end now, before the WRITE.
	time.Sleep(100 * time.Millisecond)
	close(release)
	if status := client.receive(t, messages[0].Header).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	read, status := decodeReply(t, client.receive(t, messages[1].Header), wire.DecodeReadResponse)
	if status != smb.StatusSuccess || string(read.Data) != string(data) {
		t.Fatalf("READ after the WRITE = %q, %#x; want %q", read.Data, status, data)
	}
	client.noExtraReplies(t)
}

// An async reply has no tree ID. When a CREATE after a TREE_CONNECT in one
// compound waits on storage, the TREE_CONNECT keeps its own reply with the
// new tree ID.
func TestTreeConnectCompoundWithAsyncMember(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	release := make(chan struct{})
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Open = func(ctx context.Context, object smb.Inode, access smb.Access) (smb.Handle, error) {
			select {
			case <-release:
				return srv.storage.Open(ctx, object, access)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	tree := message(t, client, wire.TreeConnect, wire.EncodeTreeConnectRequest, wire.TreeConnectRequest{Path: `\\host\backup`})
	create := related(createMessage(t, client, "file", fileOpenIf))
	if err := client.raw.Send(t.Context(), []wire.Message{tree, create}); err != nil {
		t.Fatal(err)
	}
	reply := client.next(t, tree.Header)
	close(release)
	if header := reply.Header; header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 || header.TreeID == 0 {
		t.Fatalf("TREE_CONNECT reply %+v, want a final reply with the tree ID", header)
	}
	if status := client.receive(t, create.Header).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("CREATE status %#x", status)
	}
}

// A related member that needs a file fails with the error of the member
// before it, but not with a warning.
func TestCompoundErrors(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	queryAll := func(length uint32) wire.Message {
		request := wire.QueryInfoRequest{ID: placeholder, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileAll), OutputLength: length}
		return related(message(t, client, wire.QueryInfo, wire.EncodeQueryInfoRequest, request))
	}
	for _, test := range []struct {
		name     string
		messages []wire.Message
		want     []smb.Status
	}{
		{
			"failed CREATE",
			[]wire.Message{createMessage(t, client, "missing", fileOpen), queryAll(4096), related(closeMessage(t, client, placeholder))},
			[]smb.Status{smb.StatusObjectNameNotFound, smb.StatusObjectNameNotFound, smb.StatusObjectNameNotFound},
		},
		{
			"truncated QUERY_INFO",
			[]wire.Message{createMessage(t, client, "file", fileOpenIf), queryAll(100), related(closeMessage(t, client, placeholder))},
			[]smb.Status{smb.StatusSuccess, smb.StatusBufferOverflow, smb.StatusSuccess},
		},
		{
			"no file before",
			[]wire.Message{echoMessage(t, client), related(closeMessage(t, client, placeholder))},
			[]smb.Status{smb.StatusSuccess, smb.StatusInvalidParameter},
		},
	} {
		if statuses := sendCompound(t, client, test.messages...); !slices.Equal(statuses, test.want) {
			t.Errorf("%s: statuses %#x, want %#x", test.name, statuses, test.want)
		}
	}
}

// A compound the server cannot run as a whole is refused before any member
// runs, and the connection goes on.
func TestCompoundRefusals(t *testing.T) {
	srv := newTestServer(t)
	client := srv.dial(t, smbtest.LoginOptions{Signing: smb.SigningGMAC})
	invalid := []smb.Status{smb.StatusInvalidParameter, smb.StatusInvalidParameter}
	for name, last := range map[string]func() wire.Message{
		"body does not decode": func() wire.Message {
			bad := echoMessage(t, client)
			bad.Body = []byte{0, 0, 0, 0}
			return bad
		},
		"SESSION_SETUP inside": func() wire.Message {
			return wire.Message{Header: client.header(wire.SessionSetup, 1), Body: loginStart(t)}
		},
		"credit charge below size": func() wire.Message {
			return message(t, client, wire.Write, wire.EncodeWriteRequest, wire.WriteRequest{Data: make([]byte, 64<<10+1)})
		},
	} {
		if statuses := sendCompound(t, client, createMessage(t, client, "made", fileCreateDisposition), last()); !slices.Equal(statuses, invalid) {
			t.Errorf("%s: statuses %#x", name, statuses)
		}
	}
	if srv.exists(t, "made") {
		t.Error("a refused compound created a file")
	}
	// A related first member has nothing to relate to: its chain fails, the
	// unrelated member after it runs.
	first := echoMessage(t, client)
	first.Header.Flags |= wire.FlagRelated
	statuses := sendCompound(t, client, first, related(echoMessage(t, client)), echoMessage(t, client))
	if want := []smb.Status{smb.StatusInvalidParameter, smb.StatusInvalidParameter, smb.StatusSuccess}; !slices.Equal(statuses, want) {
		t.Fatalf("statuses %#x", statuses)
	}
}

// Replies in a compound start on 8-byte boundaries, and an error reply has
// the 9-byte error body (MS-SMB2 2.2.2, 3.3.4.1.3).
func TestReplyFraming(t *testing.T) {
	client := newTestServer(t).accept(t)
	client.negotiate(t)
	// Without a session CREATE fails.
	if err := client.raw.Send(t.Context(), []wire.Message{echoMessage(t, client), createMessage(t, client, "file", fileOpenIf)}); err != nil {
		t.Fatal(err)
	}
	reply, err := client.raw.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw := reply.Raw
	if len(raw) != 72+64+9 {
		t.Fatalf("compound reply of %d bytes", len(raw))
	}
	for _, field := range []struct {
		name         string
		offset, size int
		want         uint32
	}{
		{"next command", 20, 4, 72},
		{"padding", 68, 4, 0},
		{"CREATE status", 72 + 8, 4, uint32(smb.StatusUserSessionDeleted)},
		{"last next command", 72 + 20, 4, 0},
		{"error structure size", 72 + 64, 2, 9},
		{"error context count and reserved", 72 + 66, 2, 0},
		{"error byte count", 72 + 68, 4, 0},
	} {
		var got uint32
		if field.size == 2 {
			got = uint32(binary.LittleEndian.Uint16(raw[field.offset:]))
		} else {
			got = binary.LittleEndian.Uint32(raw[field.offset:])
		}
		if got != field.want {
			t.Errorf("%s at %d = %#x, want %#x", field.name, field.offset, got, field.want)
		}
	}
}
