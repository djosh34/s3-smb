package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func placeholderFileID() wire.FileID {
	return wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}
}

func compoundFileRequest(t *testing.T, session smbtest.Session, command wire.Command, messageID uint64, id wire.FileID, related bool) wire.Message {
	t.Helper()
	var body []byte
	var err error
	switch uint16(command) {
	case uint16(wire.Flush):
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{ID: id})
	case uint16(wire.Close):
		body, err = wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	case uint16(wire.Create):
		body, err = wire.EncodeCreateRequest(wire.CreateRequest{Name: "file"})
	case uint16(wire.Echo):
		body, err = wire.EncodeEchoRequest(wire.EmptyRequest{})
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: 1})
	case uint16(wire.QueryInfo):
		body, err = wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{ID: id, OutputLength: 1})
	case uint16(wire.QueryDirectory):
		body, err = wire.EncodeQueryDirectoryRequest(wire.QueryDirectoryRequest{ID: id, OutputLength: 1})
	default:
		t.Fatalf("no test body for command %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	header := wire.Header{Command: command, MessageID: messageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}
	if related {
		header.Flags = wire.FlagRelated
		header.SessionID, header.TreeID = ^uint64(0), ^uint32(0)
	}
	return wire.Message{Header: header, Body: body}
}

type fileIDCase struct {
	name      string
	prefix    wire.Command
	async     bool
	skip      bool
	unrelated bool
	want      smb.Status
}

// Regression for #145: a signed FLUSH on an existing open supplies the FileId
// for a related FLUSH. CREATE and delayed FLUSH use the same seam.
func TestCompoundFileIDInheritance(t *testing.T) {
	for _, test := range []fileIDCase{
		{name: "existing", prefix: wire.Flush},
		{name: "existing_async", prefix: wire.Flush, async: true},
		{name: "created", prefix: wire.Create},
		{name: "unset_preserves", prefix: wire.Flush, skip: true},
		{name: "unset_preserves_async", prefix: wire.Flush, async: true, skip: true},
		{name: "missing", prefix: wire.Echo, want: smb.StatusInvalidParameter},
		{name: "unrelated_placeholder", prefix: wire.Flush, unrelated: true, want: smb.StatusFileClosed},
	} {
		t.Run(test.name, func(t *testing.T) { checkCompoundFileID(t, test) })
	}
}

func checkCompoundFileID(t *testing.T, test fileIDCase) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	options.Storage = &cleanupStorage{}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	used := make(chan wire.FileID, 3)
	created := make(chan wire.FileID, 1)
	server.handlers[wire.Flush] = testFlushHandler(test.async, release, used)
	server.handlers[wire.Create] = testCreateHandler(t, created)
	client, ctx, session := loginClient(t, server, 0, smb.SigningGMAC)
	open := insertSessionOpen(t, server, session, false, 2)
	id := wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}
	messages := []wire.Message{compoundFileRequest(t, session, test.prefix, session.NextMessageID, id, false)}
	if test.skip {
		messages = append(messages, compoundFileRequest(t, session, wire.Echo, session.NextMessageID+1, wire.FileID{}, true))
	}
	messages = append(messages, compoundFileRequest(t, session, wire.Flush, session.NextMessageID+uint64(len(messages)), placeholderFileID(), !test.unrelated))
	if err := client.Send(ctx, messages); err != nil {
		t.Fatal(err)
	}
	if test.async {
		readCompoundPending(ctx, t, client, messages)
		close(release)
	}
	statuses := compoundFinalStatuses(ctx, t, client, len(messages))
	for index, message := range messages {
		want := smb.StatusSuccess
		if index == len(messages)-1 {
			want = test.want
		}
		if statuses[message.Header.MessageID] != want {
			t.Fatalf("member %d: %v, want %v", index, statuses[message.Header.MessageID], want)
		}
	}
	if test.prefix == wire.Create {
		id = <-created
	}
	if test.prefix == wire.Flush {
		if got := <-used; got != id {
			t.Fatalf("existing ID: %+v, want %+v", got, id)
		}
	}
	if test.want == smb.StatusInvalidParameter {
		return
	}
	if test.unrelated {
		id = placeholderFileID()
	}
	if got := <-used; got != id {
		t.Fatalf("resolved ID: %+v, want %+v", got, id)
	}
}

func testFlushHandler(delayed bool, release <-chan struct{}, used chan<- wire.FileID) handler {
	return func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		if delayed && message.Header.Flags&wire.FlagRelated == 0 {
			select {
			case <-release:
			case <-ctx.Done():
				return reply{}, ctx.Err()
			}
		}
		decoded, err := wire.DecodeFlushRequest(message)
		if err != nil {
			return reply{}, err
		}
		id, status := request.FileID(decoded.ID)
		if status != smb.StatusSuccess {
			return reply{status: status}, nil
		}
		used <- id
		_, status = request.Opens.Find(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, request.Binding())
		if status != smb.StatusSuccess {
			return reply{status: status}, nil
		}
		body, err := wire.EncodeFlushResponse(wire.EmptyResponse{})
		return reply{body: body, fileID: id}, err
	}
}

func testCreateHandler(t *testing.T, created chan<- wire.FileID) handler {
	t.Helper()
	return func(_ context.Context, request RequestContext, _ wire.Message) (reply, error) {
		object := smb.ObjectKey{Inode: 3}
		reservation, status := request.Opens.Reserve(state.OpenRequest{Object: object, Binding: request.Binding(), GrantedAccess: 1, Sharing: 7})
		if status != smb.StatusSuccess {
			return reply{status: status}, nil
		}
		open, status := request.Opens.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}})
		if status != smb.StatusSuccess {
			if abortStatus := request.Opens.Abort(reservation); abortStatus != smb.StatusSuccess {
				t.Errorf("abort reservation: %v", abortStatus)
			}
			return reply{status: status}, nil
		}
		id := wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}
		created <- id
		body, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: id})
		return reply{body: body, fileID: id}, err
	}
}

func readCompoundPending(ctx context.Context, t *testing.T, client *smbtest.Client, messages []wire.Message) {
	t.Helper()
	for _, message := range messages {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 || response.Messages[0].Header.Status != smb.StatusPending || response.Messages[0].Header.MessageID != message.Header.MessageID {
			t.Fatalf("pending: %+v", response.Messages)
		}
	}
}

func compoundFinalStatuses(ctx context.Context, t *testing.T, client *smbtest.Client, count int) map[uint64]smb.Status {
	t.Helper()
	statuses := make(map[uint64]smb.Status)
	for len(statuses) < count {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range response.Messages {
			if message.Header.Status == smb.StatusPending {
				continue
			}
			if _, exists := statuses[message.Header.MessageID]; exists {
				t.Fatalf("duplicate final reply: %+v", message.Header)
			}
			statuses[message.Header.MessageID] = message.Header.Status
		}
	}
	return statuses
}

func TestCompoundFailedPredecessor(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) { checkCompoundFailedPredecessor(t, async) })
	}
}

func checkCompoundFailedPredecessor(t *testing.T, async bool) {
	t.Helper()
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	prefix := wire.Close // CLOSE cannot switch to the async path.
	if async {
		prefix = wire.Flush
	}
	server.handlers[prefix] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		if async {
			select {
			case <-release:
			case <-ctx.Done():
				return reply{}, ctx.Err()
			}
		}
		return reply{status: smb.StatusFileLockConflict}, nil
	}
	server.handlers[wire.Read] = func(_ context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		t.Error("file handler ran after a failed predecessor")
		return reply{}, nil
	}
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 3))
	session := smbtest.Session{SessionID: 77, TreeID: 12}
	messages := []wire.Message{
		compoundFileRequest(t, session, prefix, 1, wire.FileID{}, false),
		compoundFileRequest(t, session, wire.Read, 2, placeholderFileID(), true),
		compoundFileRequest(t, session, wire.Echo, 3, wire.FileID{}, true),
	}
	if err := client.Send(ctx, messages); err != nil {
		t.Fatal(err)
	}
	if async {
		readCompoundPending(ctx, t, client, messages)
		close(release)
	}
	statuses := compoundFinalStatuses(ctx, t, client, 3)
	if statuses[1] != smb.StatusFileLockConflict || statuses[2] != smb.StatusFileLockConflict || statuses[3] != smb.StatusSuccess {
		t.Fatalf("predecessor statuses: %v", statuses)
	}
}

func TestCommandsNeedingFileID(t *testing.T) {
	for _, command := range []wire.Command{wire.Close, wire.Read, wire.Write, wire.Flush, wire.Lock, wire.IOCTL, wire.QueryDirectory, wire.ChangeNotify, wire.QueryInfo, wire.SetInfo} {
		if !needsFileID(command) {
			t.Errorf("command %d must inherit predecessor errors", command)
		}
	}
	for _, command := range []wire.Command{wire.Create, wire.Echo, wire.Negotiate, wire.SessionSetup, wire.TreeConnect, wire.TreeDisconnect, wire.Logoff, wire.Cancel, wire.OplockBreak} {
		if needsFileID(command) {
			t.Errorf("command %d does not need a FileId", command)
		}
	}
}
