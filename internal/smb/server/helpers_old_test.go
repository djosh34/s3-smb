package server

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// From append_write_test.go.
// Pause after handler authorization, before the real adapter takes its inode lock.
type pausedAppendStorage struct {
	smb.Storage
	entered, resume chan struct{}
	once            sync.Once
}

// From append_write_test.go.
func (storage *pausedAppendStorage) WriteAt(ctx context.Context, handle smb.Handle, data []byte, offset uint64) (int, error) {
	if string(data) == "stale" {
		storage.once.Do(func() { close(storage.entered) })
		select {
		case <-storage.resume:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return storage.Storage.WriteAt(ctx, handle, data, offset)
}

// From append_write_test.go.
func checkStaleAppendWrite(t *testing.T, access uint32, stream bool) {
	t.Helper()
	fixture := newIOFixture(t, nil)
	storage := &pausedAppendStorage{Storage: fixture.adapter, entered: make(chan struct{}), resume: make(chan struct{})}
	client := newReadWriteClient(t, storage)
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(storage.resume) }) })
	base := createdFile(t, client.create(t, createRequest("append", fileCreateDisposition)))
	path := "append"
	initial := base
	var sibling wire.CreateResponse
	if stream {
		writeCreatedFile(t, client, base.ID, "base payload")
		path = "append:fork"
		initial = createdFile(t, client.create(t, createRequest(path, fileCreateDisposition)))
		sibling = createdFile(t, client.create(t, createRequest("append:other", fileCreateDisposition)))
		writeCreatedFile(t, client, sibling.ID, "other stream")
	}
	writeCreatedFile(t, client, initial.ID, "seed")
	request := createRequest(path, fileOpen)
	request.DesiredAccess = fileReadData | fileAppendData
	appender := createdFile(t, client.create(t, request))
	request.DesiredAccess = access
	winner := createdFile(t, client.create(t, request))
	other := createdFile(t, client.create(t, createRequest("unrelated", fileCreateDisposition)))

	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: appender.ID, Offset: 4, Data: []byte("stale")})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.Write, body, 1)
	client.next++
	if err = client.client.Send(client.ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-storage.entered:
	case <-client.ctx.Done():
		t.Fatal(client.ctx.Err())
	}
	pending, err := client.client.Receive(client.ctx)
	if err != nil || len(pending.Messages) != 1 {
		t.Fatalf("pending response: %+v, %v", pending, err)
	}
	requireIOStatus(t, pending.Messages[0], smb.StatusPending)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: winner.ID, Offset: 4, Data: []byte("winner")}, 1), smb.StatusSuccess)
	writeCreatedFile(t, client, other.ID, "other")
	readCreatedFile(t, client, other.ID, "other")
	unblock.Do(func() { close(storage.resume) })
	final, err := client.client.Receive(client.ctx)
	if err != nil || len(final.Messages) != 1 {
		t.Fatalf("final response: %+v, %v", final, err)
	}
	readCreatedFile(t, client, initial.ID, "seedwinner")
	requireIOStatus(t, final.Messages[0], smb.StatusAccessDenied)
	if final.Messages[0].Header.MessageID != message.Header.MessageID || final.Messages[0].Header.AsyncID != pending.Messages[0].Header.AsyncID {
		t.Fatal("append completion lost its request identity")
	}
	if stream {
		readCreatedFile(t, client, base.ID, "base payload")
		readCreatedFile(t, client, sibling.ID, "other stream")
	}
}

// From async_test.go.
func asyncMessage(t *testing.T, command wire.Command, id uint64) wire.Message {
	t.Helper()
	var body []byte
	var err error
	switch uint16(command) {
	case uint16(wire.Create):
		body, err = wire.EncodeCreateRequest(wire.CreateRequest{Name: "file"})
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{Length: 16})
	case uint16(wire.Write):
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{Data: []byte("data")})
	case uint16(wire.Flush):
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{})
	default:
		t.Fatalf("not an async command: %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: command, MessageID: id, SessionID: 77, TreeID: 12, CreditCharge: 1, Credit: 16}, Body: body}
}

// From async_test.go.
func controlledAsync(t *testing.T, command wire.Command, result reply, resultErr error) (*Server, chan struct{}) {
	t.Helper()
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	// Install controlled work at the handler boundary, before ServeConn starts.
	// corePipeClient supplies identity; these tests do not exercise login.
	server.handlers[command] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		select {
		case <-release:
			return result, resultErr
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	return server, release
}

// From async_test.go.
func commandName(command wire.Command) string {
	switch uint16(command) {
	case uint16(wire.Create):
		return "create"
	case uint16(wire.Read):
		return "read"
	case uint16(wire.Write):
		return "write"
	case uint16(wire.Flush):
		return "flush"
	default:
		return "unknown"
	}
}

// From cancel_protection_test.go.
func checkCancelProtection(t *testing.T, mode string) {
	t.Helper()
	server, release := controlledAsync(t, wire.Flush, reply{status: smb.StatusFileLockConflict}, nil)
	cipher := uint16(0)
	server.options.Encryption = AllowPlaintext
	if mode == "encrypted" || mode == "unencrypted" {
		cipher = smb.CipherAES256GCM
		server.options.Encryption = RequireEncryption
	}
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	request := asyncMessage(t, wire.Flush, session.NextMessageID)
	request.Header.SessionID, request.Header.TreeID = session.SessionID, session.TreeID
	pending := exchange(ctx, t, client, request)[0]
	cancel := cancelMessage(t, pending.Header, true)
	if mode == "signed" || mode == "encrypted" {
		if err := client.Send(ctx, []wire.Message{cancel}); err != nil {
			t.Fatal(err)
		}
	} else {
		if mode == "bad_signature" || mode == "unknown_signed_session" {
			cancel.Header.Flags |= wire.FlagSigned
			cancel.Header.Signature[0] = 1
		}
		if mode == "unknown_signed_session" {
			cancel.Header.SessionID++
		}
		payload, err := wire.Join([]wire.Message{cancel})
		if err != nil {
			t.Fatal(err)
		}
		sendPayload(ctx, t, client, payload)
	}
	accepted := mode == "unsigned" || mode == "signed" || mode == "encrypted"
	if accepted {
		final, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncFinal(t, final.Messages[0], pending, smb.StatusCancelled)
	}
	// Invalid signatures and unsigned CANCEL do not close the connection.
	// Only ECHO consumes the next sequence number or produces this reply.
	response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+1))[0]
	if response.Header.Command != wire.Echo || response.Header.Status != smb.StatusSuccess {
		t.Fatalf("CANCEL replied or dropped the connection: %+v", response.Header)
	}
	if !accepted {
		close(release)
		final, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncFinal(t, final.Messages[0], pending, smb.StatusFileLockConflict)
	}
}

// From cancel_test.go.
func cancelMessage(t *testing.T, target wire.Header, asynchronous bool) wire.Message {
	t.Helper()
	body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	header := wire.Header{Command: wire.Cancel, MessageID: target.MessageID, SessionID: target.SessionID}
	if asynchronous {
		header.Flags, header.AsyncID = wire.FlagAsync, target.AsyncID
		// An async CANCEL must use AsyncId, not this unrelated MessageId.
		header.MessageID = ^uint64(0)
	}
	return wire.Message{Header: header, Body: body}
}

// From cancel_test.go.
func assertAsyncFinal(t *testing.T, final, pending wire.Message, status smb.Status) {
	t.Helper()
	header, saved := final.Header, pending.Header
	if header.Status != status || header.MessageID != saved.MessageID || header.SessionID != saved.SessionID || header.AsyncID != saved.AsyncID || header.Flags&wire.FlagAsync == 0 || header.Credit != 0 || header.Command != saved.Command {
		t.Fatalf("final identity/status/credits: %+v, pending: %+v", header, saved)
	}
	if status == smb.StatusCancelled {
		if _, err := wire.DecodeErrorResponse(final); err != nil {
			t.Fatal(err)
		}
	}
}

// From cancel_test.go.
func checkCancelIdentity(t *testing.T, asynchronous, compound bool) {
	t.Helper()
	server, _ := controlledAsync(t, wire.Read, reply{}, nil)
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
	cancel := cancelMessage(t, pending.Header, asynchronous)
	// CANCEL neither consumes this credit nor validates its charge.
	cancel.Header.CreditCharge, cancel.Header.Credit = ^uint16(0), ^uint16(0)
	if compound {
		cancel.Header.Flags |= wire.FlagRelated
		cancel.Header.SessionID = ^uint64(0)
		prefix := echo(t, 2)
		prefix.Header.SessionID = 77
		if err := client.Send(ctx, []wire.Message{prefix, cancel}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := client.Send(ctx, []wire.Message{cancel}); err != nil {
			t.Fatal(err)
		}
		if err := client.Send(ctx, []wire.Message{echo(t, 2)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[uint64]bool)
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatalf("CANCEL replied in compound: %+v", response.Messages)
		}
		message := response.Messages[0]
		if seen[message.Header.MessageID] {
			t.Fatal("duplicate final reply")
		}
		seen[message.Header.MessageID] = true
		switch message.Header.MessageID {
		case 1:
			assertAsyncFinal(t, message, pending, smb.StatusCancelled)
		case 2:
			if message.Header.Command != wire.Echo || message.Header.Status != smb.StatusSuccess {
				t.Fatal(message.Header)
			}
		default:
			t.Fatalf("CANCEL got a reply: %+v", message.Header)
		}
	}
	if response := exchange(ctx, t, client, echo(t, 3))[0]; response.Header.Command != wire.Echo || response.Header.MessageID != 3 {
		t.Fatal("extra completion")
	}
}

// From capabilities_test.go.
type capabilityClient struct {
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

// From capabilities_test.go.
func newCapabilityServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// From capabilities_test.go.
func capabilityConnection(t *testing.T, server *Server) *capabilityClient {
	t.Helper()
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	return &capabilityClient{client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

// From capabilities_test.go.
func (client *capabilityClient) exchange(t *testing.T, command wire.Command, body []byte, status smb.Status) wire.Message {
	t.Helper()
	message := wire.Message{Header: wire.Header{
		Command: command, MessageID: client.next, CreditCharge: 1, Credit: 1,
		SessionID: client.session.SessionID, TreeID: client.session.TreeID,
	}, Body: body}
	client.next++
	response := ioRoundTrip(client.ctx, t, client.client, message)
	if response.Header.Status != status {
		t.Fatalf("%v response = %+v, want status %#x", command, response, status)
	}
	return response
}

// From capabilities_test.go.
func (client *capabilityClient) create(t *testing.T, request wire.CreateRequest, status smb.Status) wire.CreateResponse {
	t.Helper()
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.Create, body, status)
	if status != smb.StatusSuccess {
		return wire.CreateResponse{}
	}
	created, err := wire.DecodeCreateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// From capabilities_test.go.
func (client *capabilityClient) close(t *testing.T, id wire.FileID) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.Close, body, smb.StatusSuccess)
	if _, err := wire.DecodeCloseResponse(response); err != nil {
		t.Fatal(err)
	}
}

// From capabilities_test.go.
func (client *capabilityClient) fileInformation(t *testing.T, id wire.FileID, class wire.FileInfoClass) []byte {
	t.Helper()
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{
		ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), OutputLength: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.QueryInfo, body, smb.StatusSuccess)
	data, err := wire.DecodeQueryInfoResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return data.Data
}

// From capabilities_test.go.
func (client *capabilityClient) filesystemAttributes(t *testing.T) uint32 {
	t.Helper()
	root := client.create(t, wire.CreateRequest{
		Disposition: fileOpen, Options: fileDirectoryFile, ShareAccess: 7, DesiredAccess: 0x00120089,
	}, smb.StatusSuccess)
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{
		ID: root.ID, InfoType: wire.InfoFilesystem, InfoClass: uint8(wire.ClassFilesystemAttribute), OutputLength: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.QueryInfo, body, smb.StatusSuccess)
	data, err := wire.DecodeQueryInfoResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := wire.DecodeFilesystemAttributeInformation(data.Data)
	if err != nil {
		t.Fatal(err)
	}
	client.close(t, root.ID)
	return attributes.Attributes
}

// From change_notify_test.go.
func notifyServer(t *testing.T, cipher uint16) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// From change_notify_test.go.
func insertNotifyDirectory(t *testing.T, server *Server, session smbtest.Session) state.Open {
	t.Helper()
	object := smb.ObjectKey{Inode: 2}
	reservation, status := server.options.State.Reserve(state.OpenRequest{
		Object: object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID},
		User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: state.GUID{2},
		GrantedAccess: 1, Sharing: 7,
	})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}, Directory: true})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

// From change_notify_test.go.
func notifyMessage(t *testing.T, open state.Open, id uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeChangeNotifyRequest(wire.ChangeNotifyRequest{
		ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, OutputLength: 4096, Filter: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{
		Command: wire.ChangeNotify, MessageID: id, SessionID: open.Binding.SessionID,
		TreeID: open.Binding.TreeID, CreditCharge: 1, Credit: 1,
	}, Body: body}
}

// From change_notify_test.go.
func checkNotifyRefusal(t *testing.T, response, request wire.Message, status smb.Status) {
	t.Helper()
	header := response.Header
	if header.Command != wire.ChangeNotify || header.Status != status || header.MessageID != request.Header.MessageID ||
		header.SessionID != request.Header.SessionID || header.TreeID != request.Header.TreeID ||
		header.Flags&wire.FlagResponse == 0 || header.Flags&wire.FlagAsync != 0 || header.AsyncID != 0 ||
		header.CreditCharge != request.Header.CreditCharge || header.Credit != max(uint16(1), request.Header.Credit) {
		t.Fatalf("CHANGE_NOTIFY refusal: %+v", header)
	}
	checkNotifyErrorBody(t, response)
}

// From change_notify_test.go.
func checkNotifyErrorBody(t *testing.T, response wire.Message) {
	t.Helper()
	body, err := wire.DecodeErrorResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if body.ContextCount != 0 || len(body.Data) != 0 {
		t.Fatalf("CHANGE_NOTIFY error body: %+v", body)
	}
}

// From change_notify_test.go.
func checkNotifyOpen(t *testing.T, server *Server, open state.Open) {
	t.Helper()
	current, status := server.options.State.Find(open.ID, open.Binding)
	if status != smb.StatusSuccess || !reflect.DeepEqual(current, open) {
		t.Fatalf("CHANGE_NOTIFY changed the open: %+v, %v", current, status)
	}
}

// From change_notify_test.go.
func checkNotifyState(t *testing.T, server *Server, open state.Open) {
	t.Helper()
	checkNotifyOpen(t, server, open)
	server.mu.Lock()
	defer server.mu.Unlock()
	for connection := range server.connections {
		connection.pendingMu.Lock()
		count := len(connection.pending)
		connection.pendingMu.Unlock()
		if count != 0 {
			t.Errorf("CHANGE_NOTIFY kept %d pending requests", count)
		}
	}
}

// From change_notify_test.go.
func checkFollowingEcho(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64) {
	t.Helper()
	response := exchange(ctx, t, client, sessionEcho(t, session, id))
	if len(response) != 1 || response[0].Header.Command != wire.Echo || response[0].Header.MessageID != id || response[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("extra reply before following ECHO: %+v", response)
	}
}

// From change_notify_test.go.
func notifyProtectionName(cipher uint16) string {
	if cipher == 0 {
		return "signed"
	}
	return "encrypted"
}

// From change_notify_test.go.
func checkNotifyCompound(t *testing.T, cipher uint16, notifyFirst bool) {
	t.Helper()
	server := notifyServer(t, cipher)
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	open := insertNotifyDirectory(t, server, session)
	notify := notifyMessage(t, open, session.NextMessageID)
	echo := sessionEcho(t, session, session.NextMessageID)
	echo.Header.TreeID = session.TreeID
	requests := []wire.Message{echo, notify}
	if notifyFirst {
		requests = []wire.Message{notify, echo}
	}
	requests[1].Header.MessageID++
	requests[1].Header.Flags = wire.FlagRelated
	requests[1].Header.SessionID, requests[1].Header.TreeID = ^uint64(0), ^uint32(0)
	responses := exchange(ctx, t, client, requests...)
	if len(responses) != 2 {
		t.Fatalf("compound reply members: %d", len(responses))
	}
	for index, response := range responses {
		request := requests[index]
		request.Header.SessionID, request.Header.TreeID = session.SessionID, session.TreeID
		if response.Header.Command != request.Header.Command || response.Header.MessageID != request.Header.MessageID || response.Header.Flags&wire.FlagAsync != 0 || response.Header.Credit != 1 {
			t.Fatalf("compound member %d: %+v", index, response.Header)
		}
		if response.Header.Command == wire.ChangeNotify {
			checkNotifyRefusal(t, response, request, smb.StatusNotSupported)
		} else if response.Header.Status != smb.StatusSuccess {
			t.Fatalf("compound ECHO: %+v", response.Header)
		}
	}
	if responses[0].Header.NextCommand == 0 || responses[1].Header.NextCommand != 0 {
		t.Fatal("compound response links are wrong")
	}
	checkFollowingEcho(ctx, t, client, session, session.NextMessageID+2)
	checkNotifyState(t, server, open)
}

// From change_notify_test.go.
func checkRelatedNotify(t *testing.T, cipher uint16, status smb.Status) {
	t.Helper()
	server := notifyServer(t, cipher)
	release := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		decoded, err := wire.DecodeReadRequest(message)
		if err != nil {
			return reply{}, err
		}
		id, resolved := request.FileID(decoded.ID)
		if resolved != smb.StatusSuccess {
			return reply{status: resolved}, nil
		}
		select {
		case <-release:
			body, err := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			return reply{body: body, status: status, fileID: id}, err
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	var calls atomic.Int32
	server.handlers[wire.ChangeNotify] = func(context.Context, RequestContext, wire.Message) (reply, error) {
		calls.Add(1)
		return reply{}, nil
	}
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	open := insertNotifyDirectory(t, server, session)
	read := compoundFileRequest(t, session, wire.Read, session.NextMessageID+1,
		wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, false)
	notify := notifyMessage(t, open, session.NextMessageID+2)
	body, err := wire.EncodeChangeNotifyRequest(wire.ChangeNotifyRequest{ID: placeholderFileID(), OutputLength: 4096, Filter: 1})
	if err != nil {
		t.Fatal(err)
	}
	notify.Body = body
	notify.Header.Flags = wire.FlagRelated
	notify.Header.SessionID, notify.Header.TreeID = ^uint64(0), ^uint32(0)
	prefix := sessionEcho(t, session, session.NextMessageID)
	if sendErr := client.Send(ctx, []wire.Message{prefix, read, notify}); sendErr != nil {
		t.Fatal(sendErr)
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 || response.Messages[0].Header.Command != wire.Echo ||
		response.Messages[0].Header.MessageID != prefix.Header.MessageID || response.Messages[0].Header.Status != smb.StatusSuccess ||
		response.Messages[0].Header.NextCommand != 0 || response.Messages[0].Header.Credit != 1 {
		t.Fatalf("completed prefix: %+v", response.Messages)
	}
	requests := []wire.Message{read, notify}
	pending := readNotifyPending(ctx, t, client, session, requests)
	checkFollowingEcho(ctx, t, client, session, session.NextMessageID+3)
	close(release)
	readNotifyFinals(ctx, t, client, session, requests, pending, status)
	checkFollowingEcho(ctx, t, client, session, session.NextMessageID+4)
	checkNotifyOpen(t, server, open)
	if calls.Load() != 0 {
		t.Fatal("dependent CHANGE_NOTIFY ran a registered handler")
	}
}

// From change_notify_test.go.
func readNotifyPending(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, requests []wire.Message) map[uint64]uint64 {
	t.Helper()
	pending := make(map[uint64]uint64)
	for _, request := range requests {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatalf("pending reply members: %d", len(response.Messages))
		}
		header := response.Messages[0].Header
		if header.Command != request.Header.Command || header.MessageID != request.Header.MessageID || header.Status != smb.StatusPending ||
			header.SessionID != session.SessionID || header.Flags&(wire.FlagResponse|wire.FlagAsync) != wire.FlagResponse|wire.FlagAsync ||
			header.Credit != max(request.Header.CreditCharge, request.Header.Credit) || header.CreditCharge != request.Header.CreditCharge ||
			header.AsyncID == 0 || header.NextCommand != 0 || header.TreeID != 0 {
			t.Fatalf("related pending: %+v", header)
		}
		checkNotifyErrorBody(t, response.Messages[0])
		pending[header.MessageID] = header.AsyncID
	}
	if pending[requests[0].Header.MessageID] == pending[requests[1].Header.MessageID] {
		t.Fatal("related CHANGE_NOTIFY shares its predecessor's async ID")
	}
	return pending
}

// From change_notify_test.go.
func readNotifyFinals(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, requests []wire.Message, pending map[uint64]uint64, predecessorStatus smb.Status) {
	t.Helper()
	for range requests {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatalf("final reply members: %d", len(response.Messages))
		}
		message := response.Messages[0]
		header := message.Header
		asyncID, exists := pending[header.MessageID]
		if !exists || header.AsyncID != asyncID || header.SessionID != session.SessionID ||
			header.Flags&(wire.FlagResponse|wire.FlagAsync) != wire.FlagResponse|wire.FlagAsync ||
			header.Credit != 0 || header.CreditCharge != 1 || header.NextCommand != 0 || header.TreeID != 0 {
			t.Fatalf("related final: %+v", header)
		}
		delete(pending, header.MessageID)
		command, status := wire.Read, predecessorStatus
		if header.MessageID == requests[1].Header.MessageID {
			command = wire.ChangeNotify
			status = smb.StatusNotSupported
			if predecessorStatus == smb.StatusIODeviceError {
				status = predecessorStatus
			}
			checkNotifyErrorBody(t, message)
		}
		if header.Command != command || header.Status != status {
			t.Fatalf("related final status: %+v, want %v", header, status)
		}
	}
}

// From close_cleanup_test.go.
type closingStorage struct {
	pathOf func(context.Context, smb.Inode) (string, error)
	lookup func(context.Context, string) (smb.Resolved, error)
	cleanupStorage
}

// From close_cleanup_test.go.
func (storage *closingStorage) PathOf(ctx context.Context, inode smb.Inode) (string, error) {
	return storage.pathOf(ctx, inode)
}

// From close_cleanup_test.go.
func (storage *closingStorage) Lookup(ctx context.Context, path string) (smb.Resolved, error) {
	return storage.lookup(ctx, path)
}

// From close_cleanup_test.go.
type closeNamespaceFailure struct {
	pathErr    error
	lookupErr  error
	wantErr    error
	name       string
	failAt     int
	mismatch   bool
	persistent bool
}

// From close_cleanup_test.go.
func injectCloseNamespaceFailure(t *testing.T, server *Server, storage *closingStorage, test closeNamespaceFailure) {
	t.Helper()
	pathCalls, lookupCalls := 0, 0
	storage.pathOf = func(ctx context.Context, inode smb.Inode) (string, error) {
		pathCalls++
		if len(server.parents) != 0 {
			t.Error("discovery retried with a parent guard held")
		}
		if test.pathErr != nil && (pathCalls == 1 || test.persistent) {
			return "", test.pathErr
		}
		return storage.cleanupStorage.PathOf(ctx, inode)
	}
	storage.lookup = func(ctx context.Context, path string) (smb.Resolved, error) {
		lookupCalls++
		if test.lookupErr != nil && (lookupCalls == test.failAt || test.persistent && lookupCalls >= test.failAt) {
			return smb.Resolved{}, test.lookupErr
		}
		resolved, err := storage.cleanupStorage.Lookup(ctx, path)
		if test.mismatch && lookupCalls == test.failAt {
			resolved.Object.Inode++
		}
		return resolved, err
	}
}

// From close_cleanup_test.go.
func reserveCleanupOpen(t *testing.T, server *Server, deleteOnClose bool) state.Open {
	t.Helper()
	object := smb.ObjectKey{Inode: 2}
	token, status := server.options.State.Reserve(state.OpenRequest{Object: object, Binding: state.Binding{SessionID: 1, TreeID: 2}, GrantedAccess: 0x10003, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(token, state.Grant{Handle: cleanupHandle{object: object}, DeleteOnClose: deleteOnClose, DeleteName: smb.Name{Parent: 1, Base: "renamed"}})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

// From compound_cancel_test.go.
type drainCleanupStorage struct {
	*cleanupStorage
	drained <-chan struct{}
}

// From compound_cancel_test.go.
func (storage *drainCleanupStorage) Close(ctx context.Context, handle smb.Handle) error {
	select {
	case <-storage.drained:
		return storage.cleanupStorage.Close(ctx, handle)
	default:
		return errors.New("cleanup ran before the compound handler drained")
	}
}

// From compound_cancel_test.go.
func checkCompoundLogoff(ctx context.Context, t *testing.T, client *smbtest.Client, pending map[uint64]wire.Message) {
	t.Helper()
	seenLogoff := false
	count := len(pending) + 1
	for range count {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		message := response.Messages[0]
		if message.Header.Command == wire.Logoff {
			if seenLogoff || message.Header.Status != smb.StatusSuccess {
				t.Fatal(message.Header)
			}
			seenLogoff = true
			continue
		}
		saved, exists := pending[message.Header.MessageID]
		if !exists {
			t.Fatalf("unexpected or duplicate completion: %+v", message.Header)
		}
		assertAsyncFinal(t, message, saved, smb.StatusCancelled)
		delete(pending, message.Header.MessageID)
	}
	if !seenLogoff || len(pending) != 0 {
		t.Fatal("missing logoff or related completion")
	}
}

// From compound_cancel_test.go.
func waitCompoundSignal(ctx context.Context, t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(message)
	}
}

// From compound_cancel_test.go.
func checkCompoundDrain(t *testing.T, mode, stage string) {
	t.Helper()
	options := testOptions(t)
	prefixDrained, memberDrained := make(chan struct{}), make(chan struct{})
	storage := &drainCleanupStorage{cleanupStorage: &cleanupStorage{}, drained: prefixDrained}
	if stage == "running" {
		storage.drained = memberDrained
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release, entered := make(chan struct{}), make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		defer close(prefixDrained)
		select {
		case <-release:
			body, encodeErr := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			return reply{body: body}, encodeErr
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	server.handlers[wire.Flush] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		close(entered)
		<-ctx.Done()
		close(memberDrained)
		return reply{}, ctx.Err()
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	open := insertSessionOpen(t, server, session, false, 40)
	read := treeRequest(t, session, session.NextMessageID, wire.Read)
	member := asyncMessage(t, wire.Flush, session.NextMessageID+1)
	member.Header.Flags = wire.FlagRelated
	member.Header.SessionID, member.Header.TreeID = ^uint64(0), ^uint32(0)
	if sendErr := client.Send(ctx, []wire.Message{read, member}); sendErr != nil {
		t.Fatal(sendErr)
	}
	pending := make(map[uint64]wire.Message)
	for range 2 {
		response, receiveErr := client.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		message := response.Messages[0]
		if message.Header.Status != smb.StatusPending || message.Header.SessionID != session.SessionID {
			t.Fatal(message.Header)
		}
		pending[message.Header.MessageID] = message
	}
	if stage == "running" {
		close(release)
		waitCompoundSignal(ctx, t, entered, "related member never started")
		response, receiveErr := client.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		assertAsyncFinal(t, response.Messages[0], pending[read.Header.MessageID], smb.StatusSuccess)
		delete(pending, read.Header.MessageID)
	}
	switch mode {
	case "disconnect":
		if closeErr := client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		done := make(chan struct{})
		go func() { server.workers.Wait(); close(done) }()
		waitCompoundSignal(ctx, t, done, "connection cleanup did not finish")
	case "shutdown":
		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			t.Fatal("shutdown did not drain related work", shutdownErr)
		}
	case "logoff":
		if sendErr := client.Send(ctx, []wire.Message{treeRequest(t, session, session.NextMessageID+2, wire.Logoff)}); sendErr != nil {
			t.Fatal(sendErr)
		}
		checkCompoundLogoff(ctx, t, client, pending)
	}
	select {
	case <-storage.drained:
	default:
		t.Fatal("handler did not drain")
	}
	if storage.closed.Load() != 1 {
		t.Fatalf("cleanup closed %d opens, want 1", storage.closed.Load())
	}
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("cleanup left the open attached")
	}
	if stage == "waiting" {
		select {
		case <-entered:
			t.Fatal("cancelled dependent handler started")
		default:
		}
	}
}

// From compound_fileid_error_test.go.
func checkCompoundWarningClose(t *testing.T, warning smb.Status, async bool) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	server.handlers[wire.Flush] = testFlushHandler(async, release, make(chan wire.FileID, 1))
	command := wire.QueryInfo
	if warning == smb.StatusNoMoreFiles {
		command = wire.QueryDirectory
	}
	server.handlers[command] = testWarningHandler(warning)
	server.handlers[wire.Close] = testCompoundCloseHandler
	client, ctx, session := loginClient(t, server, 0, smb.SigningGMAC)
	open := insertSessionOpen(t, server, session, false, 2)
	id := wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}
	messages := []wire.Message{
		compoundFileRequest(t, session, wire.Flush, session.NextMessageID, id, false),
		compoundFileRequest(t, session, command, session.NextMessageID+1, placeholderFileID(), true),
		compoundFileRequest(t, session, wire.Close, session.NextMessageID+2, placeholderFileID(), true),
	}
	if err := client.Send(ctx, messages); err != nil {
		t.Fatal(err)
	}
	if async {
		readCompoundPending(ctx, t, client, messages)
		close(release)
	}
	statuses := compoundFinalStatuses(ctx, t, client, len(messages))
	for index, want := range []smb.Status{smb.StatusSuccess, warning, smb.StatusSuccess} {
		if got := statuses[messages[index].Header.MessageID]; got != want {
			t.Errorf("member %d: %v, want %v", index, got, want)
		}
	}
	if _, status := options.State.Find(open.ID, open.Binding); status != smb.StatusFileClosed {
		t.Errorf("open survived related CLOSE: %v", status)
	}
	if got := storage.closed.Load(); got != 1 {
		t.Errorf("storage closes: %d, want 1", got)
	}
}

// From compound_fileid_error_test.go.
func testWarningHandler(warning smb.Status) handler {
	return func(_ context.Context, request RequestContext, message wire.Message) (reply, error) {
		var id wire.FileID
		if message.Header.Command == wire.QueryInfo {
			decoded, err := wire.DecodeQueryInfoRequest(message)
			if err != nil {
				return reply{}, err
			}
			id = decoded.ID
		} else {
			decoded, err := wire.DecodeQueryDirectoryRequest(message)
			if err != nil {
				return reply{}, err
			}
			id = decoded.ID
		}
		id, status := request.FileID(id)
		if status != smb.StatusSuccess {
			return reply{status: status}, nil
		}
		body, err := wire.EncodeQueryInfoResponse(wire.QueryResponse{Data: []byte{1}})
		return reply{status: warning, body: body, fileID: id}, err
	}
}

// From compound_fileid_error_test.go.
func testCompoundCloseHandler(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	decoded, err := wire.DecodeCloseRequest(message)
	if err != nil {
		return reply{}, err
	}
	id, status := request.FileID(decoded.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	action, status := request.Opens.Close(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, request.Binding())
	if status != smb.StatusSuccess {
		return reply{status: status, fileID: id}, nil
	}
	if err = request.Cleanup(ctx, []state.CloseAction{action}); err != nil {
		return reply{fileID: id}, err
	}
	body, err := wire.EncodeCloseResponse(wire.CloseResponse{})
	return reply{body: body, fileID: id}, err
}

// From compound_fileid_error_test.go.
func checkCompoundHandlerErrorFileID(t *testing.T, async bool) {
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
	flush := testFlushHandler(async, release, used)
	client, ctx, session := loginClient(t, server, 0, smb.SigningGMAC)
	first := insertSessionOpen(t, server, session, false, 2)
	second := insertSessionOpen(t, server, session, false, 3)
	idA := wire.FileID{Persistent: first.ID.Persistent, Volatile: first.ID.Volatile}
	idB := wire.FileID{Persistent: second.ID.Persistent, Volatile: second.ID.Volatile}
	server.handlers[wire.Flush] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		result, err := flush(ctx, request, message)
		if err != nil {
			return result, err
		}
		if message.Header.MessageID == session.NextMessageID+1 {
			return result, smb.ErrIO
		}
		return result, nil
	}
	server.handlers[wire.Read] = func(_ context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		t.Error("READ ran after a failed FLUSH")
		return reply{}, nil
	}
	messages := []wire.Message{
		compoundFileRequest(t, session, wire.Flush, session.NextMessageID, idA, false),
		compoundFileRequest(t, session, wire.Flush, session.NextMessageID+1, idB, true),
		compoundFileRequest(t, session, wire.Read, session.NextMessageID+2, placeholderFileID(), true),
		compoundFileRequest(t, session, wire.Echo, session.NextMessageID+3, wire.FileID{}, true),
		compoundFileRequest(t, session, wire.Flush, session.NextMessageID+4, placeholderFileID(), true),
	}
	if err := client.Send(ctx, messages); err != nil {
		t.Fatal(err)
	}
	if async {
		readCompoundPending(ctx, t, client, messages)
		close(release)
	}
	statuses := compoundFinalStatuses(ctx, t, client, len(messages))
	for index, want := range []smb.Status{smb.StatusSuccess, smb.StatusIODeviceError, smb.StatusIODeviceError, smb.StatusSuccess, smb.StatusSuccess} {
		if got := statuses[messages[index].Header.MessageID]; got != want {
			t.Fatalf("member %d: %v, want %v", index, got, want)
		}
	}
	for index, want := range []wire.FileID{idA, idB, idB} {
		select {
		case got := <-used:
			if got != want {
				t.Errorf("FLUSH %d used %+v, want %+v", index, got, want)
			}
		default:
			t.Fatalf("FLUSH %d did not run", index)
		}
	}
}

// From compound_fileid_test.go.
func placeholderFileID() wire.FileID {
	return wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}
}

// From compound_fileid_test.go.
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

// From compound_fileid_test.go.
type fileIDCase struct {
	name      string
	prefix    wire.Command
	async     bool
	skip      bool
	unrelated bool
	want      smb.Status
}

// From compound_fileid_test.go.
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

// From compound_fileid_test.go.
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

// From compound_fileid_test.go.
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

// From compound_fileid_test.go.
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

// From compound_fileid_test.go.
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

// From compound_fileid_test.go.
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

// From compound_semantics_test.go.
func checkFirstRelatedValidation(t *testing.T, signing uint16, invalid string) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		calls.Add(1)
		return handleEcho(ctx, request, message)
	}
	client, ctx, session := loginClient(t, server, 0, signing)
	first := sessionEcho(t, session, session.NextMessageID)
	first.Header.Flags = wire.FlagRelated
	requests := []wire.Message{first}
	want := smb.StatusAccessDenied
	if invalid == "unsigned" {
		payload, encodeErr := wire.Join(requests)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		sendPayload(ctx, t, client, payload)
	} else {
		second := compoundFileRequest(t, session, wire.Echo, session.NextMessageID+1, wire.FileID{}, true)
		if invalid == "body" {
			second.Body = []byte{0, 0, 0, 0}
		} else {
			second.Header.Command = wire.Read
			second.Body, err = wire.EncodeReadRequest(wire.ReadRequest{Length: smb.CreditUnit + 1})
			if err != nil {
				t.Fatal(err)
			}
		}
		requests = append(requests, second, sessionEcho(t, session, session.NextMessageID+2))
		want = smb.StatusInvalidParameter
		if sendErr := client.Send(ctx, requests); sendErr != nil {
			t.Fatal(sendErr)
		}
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != len(requests) || calls.Load() != 0 {
		t.Fatalf("invalid compound: %d replies, %d handlers", len(response.Messages), calls.Load())
	}
	for _, message := range response.Messages {
		if message.Header.Status != want || message.Header.SessionID != session.SessionID || message.Header.Flags&wire.FlagSigned == 0 {
			t.Fatalf("invalid request lost its protected refusal: %+v", message.Header)
		}
	}
	next := session.NextMessageID + uint64(len(requests))
	if response := exchange(ctx, t, client, sessionEcho(t, session, next))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("refusal closed the connection")
	}
}

// From concurrent_cleanup_test.go.
func checkCleanupWaits(t *testing.T, pending bool) {
	t.Helper()
	connection := &connection{pending: make(map[uint64]*pendingRequest), inflight: make(map[*sessionRequest]struct{})}
	contexts := make([]context.Context, 0, 3)
	cleanupDone := make([]chan struct{}, 0, 2)
	var fileDone chan struct{}
	for index, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect, wire.Read} {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		contexts = append(contexts, ctx)
		done := make(chan struct{})
		if command == wire.Read {
			fileDone = done
			go func() { <-ctx.Done(); close(done) }()
		} else {
			cleanupDone = append(cleanupDone, done)
			defer close(done)
		}
		id := uint64(index + 1)
		header := wire.Header{Command: command, MessageID: id, SessionID: 1, TreeID: 1}
		if pending {
			connection.pending[id] = &pendingRequest{header: header, work: &work{done: done, cancel: cancel}}
		} else {
			connection.inflight[&sessionRequest{header: header, cancel: cancel, done: done}] = struct{}{}
		}
	}
	finished := make(chan struct{}, 2)
	go func() { connection.stopRequests(1, 0, 1); finished <- struct{}{} }()
	go func() { connection.stopRequests(1, 1, 2); finished <- struct{}{} }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for range 2 {
		select {
		case <-finished:
		case <-ctx.Done():
			t.Fatal("cleanup requests waited on each other")
		}
	}
	for _, ctx := range contexts {
		if ctx.Err() == nil {
			t.Fatal("cleanup did not cancel matching work")
		}
	}
	select {
	case <-fileDone:
	default:
		t.Fatal("file work was not drained")
	}
	for _, done := range cleanupDone {
		select {
		case <-done:
			t.Fatal("test did not keep cleanup work pending")
		default:
		}
	}
}

// From create_async_lease_test.go.
func checkPendingCreateProgress(t *testing.T, command wire.Command, cipher uint16) {
	t.Helper()
	options := testOptions(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, newErr := New(options)
	if newErr != nil {
		t.Fatal(newErr)
	}
	beginBreak, progressed := make(chan struct{}), make(chan struct{})
	key := [16]byte{1, 2, 3}
	holderID := wire.FileID{Persistent: 10, Volatile: 20}
	createdID := wire.FileID{Persistent: 30, Volatile: 40}
	result := createTestReply(t, createdID)
	server.handlers[wire.Create] = func(ctx context.Context, request RequestContext, _ wire.Message) (reply, error) {
		select {
		case <-beginBreak:
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
		actions, sendErr := server.sendLeaseBreak(ctx, state.Break{Binding: request.Binding(), ClientGUID: request.Session.ClientGUID, LeaseKey: state.GUID(key), CurrentState: 7, AckRequired: true, Epoch: 1})
		if err := errors.Join(sendErr, request.Cleanup(ctx, actions)); err != nil {
			return reply{}, err
		}
		select {
		case <-progressed:
			return result, nil
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	server.handlers[command] = createTestProgressHandler(command, key, holderID, progressed)
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	request := compoundFileRequest(t, session, wire.Create, session.NextMessageID, wire.FileID{}, false)
	pending := exchange(ctx, t, client, request)[0]
	assertCreatePending(t, request, pending)
	close(beginBreak)
	decoded, err := client.WaitLeaseBreak(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Key != key || decoded.CurrentState != 7 || decoded.NewState != 0 || decoded.Flags != 1 || decoded.Epoch != 1 {
		t.Fatalf("lease break: %+v", decoded)
	}
	progress := compoundFileRequest(t, session, wire.Close, request.Header.MessageID+1, holderID, false)
	if command == wire.OplockBreak {
		progress.Header.Command = wire.OplockBreak
		body, encodeErr := wire.EncodeLeaseBreakRequest(wire.LeaseBreakRequest{Key: key})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		progress.Body = body
	}
	if err := client.Send(ctx, []wire.Message{progress}); err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint64]bool)
	for range 2 {
		response := receiveCreateTestMessage(ctx, t, client)
		id := response.Header.MessageID
		if seen[id] {
			t.Fatalf("duplicate reply: %+v", response.Header)
		}
		seen[id] = true
		switch id {
		case request.Header.MessageID:
			assertCreateFinal(t, request, pending, response, smb.StatusSuccess)
			assertCreatedFileID(t, response, createdID)
		case progress.Header.MessageID:
			if response.Header.Command != command || response.Header.Status != smb.StatusSuccess || response.Header.Flags&wire.FlagAsync != 0 {
				t.Fatalf("holder progress: %+v", response.Header)
			}
		default:
			t.Fatalf("unexpected reply: %+v", response.Header)
		}
	}
	assertCreateTestEcho(ctx, t, client, session, request.Header.MessageID+2)
}

// From create_async_lease_test.go.
func createTestProgressHandler(command wire.Command, key [16]byte, holderID wire.FileID, progressed chan<- struct{}) handler {
	return func(_ context.Context, _ RequestContext, message wire.Message) (reply, error) {
		var body []byte
		var err error
		if command == wire.OplockBreak {
			ack, decodeErr := wire.DecodeLeaseBreakRequest(message)
			if decodeErr != nil {
				return reply{}, decodeErr
			}
			if ack.Key != key || ack.State != 0 {
				return reply{}, errors.New("unexpected controlled lease acknowledgment")
			}
			body, err = wire.EncodeLeaseBreakResponse(wire.LeaseBreakResponse{Key: key})
		} else {
			closeRequest, decodeErr := wire.DecodeCloseRequest(message)
			if decodeErr != nil {
				return reply{}, decodeErr
			}
			if closeRequest.ID != holderID {
				return reply{}, errors.New("unexpected controlled holder FileId")
			}
			body, err = wire.EncodeCloseResponse(wire.CloseResponse{})
		}
		if err != nil {
			return reply{}, err
		}
		close(progressed)
		return reply{body: body}, nil
	}
}

// From create_async_test.go.
func checkCreateCancellation(t *testing.T, async bool) {
	t.Helper()
	id := wire.FileID{Persistent: 30, Volatile: 40}
	server, release := controlledAsync(t, wire.Create, createTestReply(t, id), nil)
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	first, second := asyncMessage(t, wire.Create, 1), asyncMessage(t, wire.Create, 2)
	firstPending := exchange(ctx, t, client, first)[0]
	assertCreatePending(t, first, firstPending)
	secondPending := exchange(ctx, t, client, second)[0]
	assertCreatePending(t, second, secondPending)
	if firstPending.Header.AsyncID == secondPending.Header.AsyncID {
		t.Fatal("pending CREATE requests share an async ID")
	}
	body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cancel := wire.Message{Header: wire.Header{Command: wire.Cancel, MessageID: 1, SessionID: 77}, Body: body}
	if async {
		cancel.Header.MessageID, cancel.Header.AsyncID = 0, firstPending.Header.AsyncID
		cancel.Header.Flags = wire.FlagAsync
	}
	if err := client.Send(ctx, []wire.Message{cancel, echo(t, 3)}); err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint64]bool)
	for range 2 {
		response := receiveCreateTestMessage(ctx, t, client)
		messageID := response.Header.MessageID
		if seen[messageID] {
			t.Fatalf("duplicate reply after CANCEL: %+v", response.Header)
		}
		seen[messageID] = true
		switch messageID {
		case 1:
			assertCreateFinal(t, first, firstPending, response, smb.StatusCancelled)
		case 3:
			if response.Header.Command != wire.Echo || response.Header.Status != smb.StatusSuccess || response.Header.Flags&wire.FlagAsync != 0 {
				t.Fatalf("ECHO after CANCEL: %+v", response.Header)
			}
		default:
			t.Fatalf("CANCEL replied or affected another request: %+v", response.Header)
		}
	}
	close(release)
	final := receiveCreateTestMessage(ctx, t, client)
	assertCreateFinal(t, second, secondPending, final, smb.StatusSuccess)
	assertCreatedFileID(t, final, id)
	assertCreateTestEcho(ctx, t, client, smbtest.Session{SessionID: 77}, 4)
}

// From create_async_test.go.
func createTestReply(t *testing.T, id wire.FileID) reply {
	t.Helper()
	body, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return reply{body: body, fileID: id}
}

// From create_async_test.go.
func receiveCreateTestMessage(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply, got %+v", response.Messages)
	}
	return response.Messages[0]
}

// From create_async_test.go.
func assertCreatePending(t *testing.T, request, pending wire.Message) {
	t.Helper()
	header := pending.Header
	if header.Command != request.Header.Command || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || header.Credit != 16 || header.CreditCharge != 1 || header.TreeID != 0 {
		t.Fatalf("CREATE pending identity/credits: %+v", header)
	}
}

// From create_async_test.go.
func assertCreateFinal(t *testing.T, request, pending, final wire.Message, status smb.Status) {
	t.Helper()
	header := final.Header
	if header.Command != request.Header.Command || header.MessageID != request.Header.MessageID || header.SessionID != pending.Header.SessionID || header.AsyncID != pending.Header.AsyncID || header.Flags&wire.FlagAsync == 0 || header.Status != status || header.Credit != 0 || header.CreditCharge != 1 || header.TreeID != 0 {
		t.Fatalf("CREATE final identity/credits: %+v", header)
	}
}

// From create_async_test.go.
func assertCreatedFileID(t *testing.T, response wire.Message, want wire.FileID) {
	t.Helper()
	created, err := wire.DecodeCreateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != want {
		t.Fatalf("created FileId: %+v, want %+v", created.ID, want)
	}
}

// From create_async_test.go.
func assertCreateTestEcho(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64) {
	t.Helper()
	messages := exchange(ctx, t, client, sessionEcho(t, session, id))
	if len(messages) != 1 || messages[0].Header.Command != wire.Echo || messages[0].Header.MessageID != id || messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("extra reply or failed ECHO: %+v", messages)
	}
}

// From create_caching_snapshot_test.go.
type createSnapshotStorage struct {
	smb.Storage
	entered chan struct{}
	release chan struct{}
	block   atomic.Bool
}

// From create_caching_snapshot_test.go.
func (storage *createSnapshotStorage) GetAttr(ctx context.Context, object smb.ObjectKey) (smb.Attr, error) {
	if storage.block.CompareAndSwap(true, false) {
		close(storage.entered)
		select {
		case <-storage.release:
		case <-ctx.Done():
			return smb.Attr{}, ctx.Err()
		}
	}
	return storage.Storage.GetAttr(ctx, object)
}

// From create_caching_snapshot_test.go.
func checkReplayCachingResponseUsesFreshEffectiveH(t *testing.T, acknowledged bool) {
	t.Helper()
	options := testOptions(t)
	storage := &createSnapshotStorage{Storage: newFilesMetaStorage(t), entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(storage.release) })
	t.Cleanup(unblock)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	holder, writer := loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3)
	create := durableCreateOptions()
	create.Request.Name, create.Request.DesiredAccess, create.Request.ShareAccess = "file", fileReadData, 1
	create.Lease = leaseV2(4, smb.LeaseRead|smb.LeaseHandle)
	header := holder.header(wire.Create)
	if sendErr := holder.client.SendCreate(holder.ctx, header, create); sendErr != nil {
		t.Fatal(sendErr)
	}
	initial := holder.created(t, header.MessageID)
	if initial.Durable == nil || initial.Durable.Timeout != 120000 {
		t.Fatalf("initial durable response = %+v", initial)
	}

	// A second attached member can ACK while the original transport
	// is serving the blocked replay. No connection policy change.
	acknowledger := loginCreateLeaseClient(t, server, 2)
	member := acknowledger.create(t, leaseCreateRequest("file"), leaseV2(4, smb.LeaseRead))
	storage.block.Store(true)
	replay := holder.header(wire.Create)
	replay.Flags |= wire.FlagReplay
	if sendErr := holder.client.SendCreate(holder.ctx, replay, create); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-storage.entered:
	case <-holder.ctx.Done():
		t.Fatal(holder.ctx.Err())
	}
	competing := leaseCreateRequest("file")
	competing.DesiredAccess = fileWriteData
	id := writer.send(t, competing, nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.CurrentState != smb.LeaseRead|smb.LeaseHandle || notification.NewState != smb.LeaseRead || notification.Epoch != 9 {
		t.Fatalf("H-removing break = %+v", notification)
	}
	if acknowledged {
		acknowledger.ack(t, notification)
	}
	unblock()
	result := holder.created(t, replay.MessageID)
	wantState, wantFlags := uint32(smb.LeaseRead|smb.LeaseHandle), uint32(leaseParentKeySet|leaseBreakInProgress)
	if acknowledged {
		wantState, wantFlags = smb.LeaseRead, leaseParentKeySet
	}
	if result.Reply.ID != initial.Reply.ID || result.Lease == nil || result.Lease.State != wantState || result.Lease.Epoch != 9 || result.Lease.Flags != wantFlags || result.Durable != nil {
		t.Fatalf("replay exposed stale caching/durability: %+v, lease %+v", result, result.Lease)
	}
	if !acknowledged {
		acknowledger.ack(t, notification)
	}
	refused, err := writer.receive(id)
	if err != nil || refused.Header.Status != smb.StatusSharingViolation {
		t.Fatalf("sharing retry = %#x, error %v", refused.Header.Status, err)
	}
	holder.close(t, initial.Reply.ID)
	acknowledger.close(t, member.Reply.ID)
}

// From create_close_helpers_test.go.
func newFileClient(t *testing.T) (*Server, *smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	return server, client, ctx, session
}

// From create_close_helpers_test.go.
func fileCreate(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, create wire.CreateRequest) wire.Message {
	t.Helper()
	body, err := wire.EncodeCreateRequest(create)
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.Create, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
	return ioRoundTrip(ctx, t, client, message)
}

// From create_close_helpers_test.go.
func fileClose(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, fileID wire.FileID, flags uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: fileID, Flags: flags})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.Close, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
	return ioRoundTrip(ctx, t, client, message)
}

// From create_close_helpers_test.go.
func createdFile(t *testing.T, message wire.Message) wire.CreateResponse {
	t.Helper()
	if message.Header.Status != smb.StatusSuccess {
		t.Fatalf("CREATE status = %#x", message.Header.Status)
	}
	response, err := wire.DecodeCreateResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// From create_commit_race_test.go.
// Count actual adapter references: reselection must not repeat storage Open or
// close a successfully committed handle as though it were a failed CREATE.
type createCommitRaceStorage struct {
	smb.Storage
	opens  atomic.Uint64
	closes atomic.Uint64
}

// From create_commit_race_test.go.
func (storage *createCommitRaceStorage) Open(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	handle, err := storage.Storage.Open(ctx, object, access)
	if err == nil {
		storage.opens.Add(1)
	}
	return handle, err
}

// From create_commit_race_test.go.
func (storage *createCommitRaceStorage) Close(ctx context.Context, handle smb.Handle) error {
	err := storage.Storage.Close(ctx, handle)
	if err == nil {
		storage.closes.Add(1)
	}
	return err
}

// From create_commit_race_test.go.
func checkConcurrentCreateGrant(t *testing.T, lease *wire.LeaseContext, durable *wire.DurableReply, level uint8) {
	t.Helper()
	if lease == nil || lease.ParentKey != [16]byte{8} || lease.Duration != 0 {
		t.Fatalf("invalid concurrent lease response: %+v", lease)
	}
	if lease.State == 0 {
		if level != 0 || lease.Epoch != 7 || lease.Flags != leaseParentKeySet || durable != nil {
			t.Fatalf("declined response advanced epoch/promised durability: %+v, %+v", lease, durable)
		}
		return
	}
	if lease.State != smb.LeaseRead|smb.LeaseHandle || level != leaseOplockLevel {
		t.Fatalf("unsupported concurrent grant: %+v, level %#x", lease, level)
	}
	if lease.Flags == leaseParentKeySet|leaseBreakInProgress {
		if lease.Epoch != 9 || durable != nil {
			t.Fatalf("pending NONE target promised effective H: %+v, %+v", lease, durable)
		}
		return
	}
	if lease.Flags != leaseParentKeySet || lease.Epoch != 8 || durable != nil && durable.Timeout != 120000 {
		t.Fatalf("acquisition changed shared epoch/timeout contract: %+v, %+v", lease, durable)
	}
}

// From create_dispositions_test.go.
func (client *readWriteClient) create(t *testing.T, request wire.CreateRequest) wire.Message {
	t.Helper()
	message := fileCreate(client.ctx, t, client.client, client.session, client.next, request)
	client.next++
	return message
}

// From create_dispositions_test.go.
func (client *readWriteClient) close(t *testing.T, id wire.FileID, flags uint16) wire.Message {
	t.Helper()
	message := fileClose(client.ctx, t, client.client, client.session, client.next, id, flags)
	client.next++
	return message
}

// From create_dispositions_test.go.
func createRequest(name string, disposition uint32) wire.CreateRequest {
	return wire.CreateRequest{Name: name, DesiredAccess: 0x10000000, ShareAccess: 7, Disposition: disposition}
}

// From create_dispositions_test.go.
func readCreatedFile(t *testing.T, client *readWriteClient, id wire.FileID, want string) {
	t.Helper()
	response := client.read(t, wire.ReadRequest{ID: id, Length: 64}, 1)
	if want == "" {
		requireIOStatus(t, response, smb.StatusEndOfFile)
		return
	}
	requireIOStatus(t, response, smb.StatusSuccess)
	read, err := wire.DecodeReadResponse(response)
	if err != nil || string(read.Data) != want {
		t.Fatalf("READ = %q, %v; want %q", read.Data, err, want)
	}
}

// From create_dispositions_test.go.
func writeCreatedFile(t *testing.T, client *readWriteClient, id wire.FileID, data string) {
	t.Helper()
	response := client.write(t, wire.WriteRequest{ID: id, Data: []byte(data)}, 1)
	requireIOStatus(t, response, smb.StatusSuccess)
	write, err := wire.DecodeWriteResponse(response)
	if err != nil || uint64(write.Count) != uint64(len(data)) {
		t.Fatalf("WRITE = %+v, %v", write, err)
	}
}

// From create_dispositions_test.go.
func checkCreateDisposition(t *testing.T, client *readWriteClient, disposition uint32, exists bool, replacement string) {
	t.Helper()
	var first wire.FileID
	const old = "old payload"
	if exists {
		created := createdFile(t, client.create(t, createRequest("file", fileCreateDisposition)))
		first = created.ID
		writeCreatedFile(t, client, first, old)
	}
	response := client.create(t, createRequest("file", disposition))
	if exists && disposition == fileCreateDisposition {
		requireIOStatus(t, response, smb.StatusObjectNameCollision)
		readCreatedFile(t, client, first, old)
		requireIOStatus(t, client.close(t, first, 0), smb.StatusSuccess)
		return
	}
	if !exists && (disposition == fileOpen || disposition == fileOverwrite) {
		requireIOStatus(t, response, smb.StatusObjectNameNotFound)
		return
	}
	created := createdFile(t, response)
	wantAction := uint32(2)
	before := ""
	if exists {
		switch disposition {
		case fileOpen, fileOpenIf:
			wantAction, before = 1, old
		case fileSupersede:
			wantAction = 0
		case fileOverwrite, fileOverwriteIf:
			wantAction = 3
		}
	}
	if created.Action != wantAction || created.Size != uint64(len(before)) {
		t.Fatalf("CREATE = %+v, want action %d, size %d", created, wantAction, len(before))
	}
	readCreatedFile(t, client, created.ID, before)
	if exists {
		readCreatedFile(t, client, first, before)
	}
	writeCreatedFile(t, client, created.ID, replacement)
	want := replacement
	if len(before) > len(replacement) {
		want += before[len(replacement):]
	}
	requireIOStatus(t, client.close(t, created.ID, 0), smb.StatusSuccess)
	if exists {
		readCreatedFile(t, client, first, want)
		requireIOStatus(t, client.close(t, first, 0), smb.StatusSuccess)
	}
	reopened := createdFile(t, client.create(t, createRequest("file", fileOpen)))
	readCreatedFile(t, client, reopened.ID, want)
	requireIOStatus(t, client.close(t, reopened.ID, 0), smb.StatusSuccess)
}

// From create_failure_test.go.
const (
	createFailOpen int32 = iota + 1
	createFailTruncate
	createFailGetAttr
	createFailClose
)

// From create_failure_test.go.
type createFaultStorage struct {
	smb.Storage
	failure       atomic.Int32
	opens, closes atomic.Int64
}

// From create_failure_test.go.
func (storage *createFaultStorage) Open(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	handle, err := storage.Storage.Open(ctx, object, access)
	if err != nil {
		return handle, err
	}
	storage.opens.Add(1)
	if storage.failure.Load() == createFailOpen {
		// Even a reference returned with an error must be released.
		return handle, smb.ErrIO
	}
	return handle, nil
}

// From create_failure_test.go.
func (storage *createFaultStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.closes.Add(1)
	err := storage.Storage.Close(ctx, handle)
	if storage.failure.Load() == createFailClose {
		return errors.Join(err, smb.ErrIO)
	}
	return err
}

// From create_failure_test.go.
func (storage *createFaultStorage) Truncate(ctx context.Context, handle smb.Handle, size uint64) error {
	if storage.failure.Load() == createFailTruncate {
		return smb.ErrIO
	}
	return storage.Storage.Truncate(ctx, handle, size)
}

// From create_failure_test.go.
func (storage *createFaultStorage) GetAttr(ctx context.Context, object smb.ObjectKey) (smb.Attr, error) {
	if storage.failure.Load() == createFailGetAttr {
		return smb.Attr{}, smb.ErrIO
	}
	return storage.Storage.GetAttr(ctx, object)
}

// From create_lease_async_test.go.
func checkCreateLeaseSameTransport(t *testing.T, closeHolder bool, cipher uint16) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client := loginCreateLeaseClientWithCipher(t, server, 2, cipher)
	held := leaseCreateRequest("file")
	requested := uint32(7)
	if closeHolder {
		held.ShareAccess, requested = 1, 3
	}
	opened := client.create(t, held, leaseV2(1, requested))
	other := leaseCreateRequest("file")
	if closeHolder {
		other.DesiredAccess = fileWriteData
	}
	id := client.send(t, other, leaseV2(2, 7))
	pending, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Messages) != 1 {
		t.Fatalf("pending replies = %+v", pending.Messages)
	}
	header := pending.Messages[0].Header
	if header.MessageID != id || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 {
		t.Fatalf("pending CREATE = %+v", header)
	}
	notification, err := client.client.WaitLeaseBreak(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := uint32(3)
	if closeHolder {
		target = smb.LeaseRead // OPEN_BREAK_H removes H, not ordinary R.
	}
	if notification.CurrentState != requested || notification.NewState != target || notification.Key != [16]byte{1} {
		t.Fatalf("same-transport break = %+v", notification)
	}
	if closeHolder {
		client.close(t, opened.Reply.ID)
	} else {
		client.ack(t, notification)
	}
	message, err := client.receive(id)
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.AsyncID != header.AsyncID || message.Header.Flags&wire.FlagAsync == 0 || message.Header.Credit != 0 {
		t.Fatalf("final CREATE = %+v", message.Header)
	}
	result, err := wire.DecodeCreateResponse(message)
	if err != nil || message.Header.Status != smb.StatusSuccess || result.ID == (wire.FileID{}) {
		t.Fatalf("CREATE after holder progress = %+v, error %v", message.Header, err)
	}
	client.close(t, result.ID)
}

// From create_lease_test.go.
type createLeaseClient struct {
	client  *smbtest.Client
	ctx     context.Context
	replies map[uint64]wire.Message
	session smbtest.Session
	next    uint64
}

// From create_lease_test.go.
func newCreateLeaseClients(t *testing.T) (*Server, *createLeaseClient, *createLeaseClient) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server, loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3)
}

// From create_lease_test.go.
func loginCreateLeaseClient(t *testing.T, server *Server, guid byte) *createLeaseClient {
	t.Helper()
	return loginCreateLeaseClientWithCipher(t, server, guid, smb.CipherAES128GCM)
}

// From create_lease_test.go.
func loginCreateLeaseClientWithCipher(t *testing.T, server *Server, guid byte, cipher uint16) *createLeaseClient {
	t.Helper()
	client, ctx := pipeClient(t, server)
	session, err := client.Login(ctx, smbtest.LoginOptions{
		Share: server.options.ShareName, Account: server.options.Account,
		ClientGUID: [16]byte{guid}, Cipher: cipher, Signing: smb.SigningCMAC,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &createLeaseClient{client: client, ctx: ctx, session: session, next: session.NextMessageID, replies: make(map[uint64]wire.Message)}
}

// From create_lease_test.go.
func (client *createLeaseClient) header(command wire.Command) wire.Header {
	header := wire.Header{Command: command, MessageID: client.next, SessionID: client.session.SessionID, TreeID: client.session.TreeID, CreditCharge: 1, Credit: 16}
	client.next++
	return header
}

// From create_lease_test.go.
func (client *createLeaseClient) send(t *testing.T, request wire.CreateRequest, lease *wire.LeaseContext) uint64 {
	t.Helper()
	header := client.header(wire.Create)
	if err := client.client.SendCreate(client.ctx, header, smbtest.CreateOptions{Request: request, Lease: lease}); err != nil {
		t.Fatal(err)
	}
	return header.MessageID
}

// From create_lease_test.go.
func (client *createLeaseClient) sendRawCreate(t *testing.T, request wire.CreateRequest) uint64 {
	t.Helper()
	header := client.header(wire.Create)
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := client.client.Send(client.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	return header.MessageID
}

// From create_lease_test.go.
func (client *createLeaseClient) receive(id uint64) (wire.Message, error) {
	for {
		if message, exists := client.replies[id]; exists {
			delete(client.replies, id)
			return message, nil
		}
		reply, err := client.client.Receive(client.ctx)
		if err != nil {
			return wire.Message{}, err
		}
		for _, message := range reply.Messages {
			if message.Header.Status != smb.StatusPending {
				client.replies[message.Header.MessageID] = message
			}
		}
	}
}

// From create_lease_test.go.
func (client *createLeaseClient) created(t *testing.T, id uint64) smbtest.CreateResult {
	t.Helper()
	message, err := client.receive(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := smbtest.DecodeCreateReply(message)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// From create_lease_test.go.
func (client *createLeaseClient) create(t *testing.T, request wire.CreateRequest, lease *wire.LeaseContext) smbtest.CreateResult {
	t.Helper()
	return client.created(t, client.send(t, request, lease))
}

// From create_lease_test.go.
func (client *createLeaseClient) ack(t *testing.T, notification wire.LeaseBreakNotification) {
	t.Helper()
	header := client.header(wire.OplockBreak)
	header.TreeID = 0
	if err := client.client.SendLeaseBreakAcknowledgment(client.ctx, header, wire.LeaseBreakRequest{Key: notification.Key, State: notification.NewState}); err != nil {
		t.Fatal(err)
	}
	message, err := client.receive(header.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Status != smb.StatusSuccess {
		t.Fatalf("lease ACK = %#x", message.Header.Status)
	}
	ack, err := wire.DecodeLeaseBreakResponse(message)
	if err != nil || ack.Key != notification.Key || ack.State != notification.NewState {
		t.Fatalf("lease ACK = %+v, error %v", ack, err)
	}
}

// From create_lease_test.go.
func (client *createLeaseClient) close(t *testing.T, id wire.FileID) {
	t.Helper()
	header := client.header(wire.Close)
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := client.client.Send(client.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	message, err := client.receive(header.MessageID)
	if err != nil || message.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE = %#x, error %v", message.Header.Status, err)
	}
}

// From create_lease_test.go.
func leaseCreateRequest(name string) wire.CreateRequest {
	return wire.CreateRequest{Name: name, DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpenIf}
}

// From create_lease_test.go.
func leaseV2(key byte, requested uint32) *wire.LeaseContext {
	return &wire.LeaseContext{Version: 2, Key: [16]byte{key}, State: requested, Epoch: 7, ParentKey: [16]byte{8}, Flags: leaseParentKeySet, Duration: 999}
}

// From create_lease_test.go.
func assertLeaseGrant(t *testing.T, result smbtest.CreateResult, want uint32) {
	t.Helper()
	if result.Lease == nil || result.Lease.Version != 2 || result.Lease.State != want || result.Lease.Duration != 0 {
		t.Fatalf("lease grant = %+v, want %#x", result.Lease, want)
	}
	level := uint8(0)
	if want != 0 {
		level = leaseOplockLevel
	}
	if result.Reply.OplockLevel != level {
		t.Fatalf("oplock = %#x, want %#x", result.Reply.OplockLevel, level)
	}
}

// From create_lease_test.go.
type createLeaseOutcome struct {
	err     error
	message wire.Message
}

// From create_lease_test.go.
func receiveCreateLater(client *createLeaseClient, id uint64) <-chan createLeaseOutcome {
	done := make(chan createLeaseOutcome, 1)
	go func() {
		message, err := client.receive(id)
		done <- createLeaseOutcome{message: message, err: err}
	}()
	return done
}

// From create_lease_test.go.
func assertCreateWaits(t *testing.T, done <-chan createLeaseOutcome) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("CREATE completed before the break ended: %+v, error %v", result.message.Header, result.err)
	case <-time.After(20 * time.Millisecond):
	}
}

// From create_lease_test.go.
func finishCreate(t *testing.T, done <-chan createLeaseOutcome) smbtest.CreateResult {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		decoded, err := smbtest.DecodeCreateReply(result.message)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
		return smbtest.CreateResult{}
	}
}

// From create_mutation_gate_test.go.
// This storage-boundary barrier does not know about the state mutation API.
// It keeps actual WriteAt in flight without holding an adapter inode lock.
// All successful bytes still go through the real file/SQLite adapter.
type createMutationWriteStorage struct {
	smb.Storage
	writeErr error
	entered  chan struct{}
	release  chan struct{}
}

// From create_mutation_gate_test.go.
func (storage *createMutationWriteStorage) WriteAt(ctx context.Context, handle smb.Handle, data []byte, offset uint64) (int, error) {
	close(storage.entered)
	select {
	case <-storage.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if storage.writeErr != nil {
		return 0, storage.writeErr
	}
	return storage.Storage.WriteAt(ctx, handle, data, offset)
}

// From create_mutation_gate_test.go.
func checkCreateDuringWriteWithholdsFreshReadLease(t *testing.T, outcome string) {
	t.Helper()
	options := testOptions(t)
	storage := &createMutationWriteStorage{Storage: newFilesMetaStorage(t), entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(storage.release) })
	t.Cleanup(unblock)
	if outcome == "error" {
		storage.writeErr = smb.ErrIO
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	holder, writer, reader := loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3), loginCreateLeaseClient(t, server, 4)
	holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	ordinary := leaseCreateRequest("file")
	ordinary.Disposition, ordinary.DesiredAccess = fileOpen, fileWriteData
	opened := writer.create(t, ordinary, nil)
	shared := holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	assertLeaseGrant(t, shared, smb.LeaseRead)
	if shared.Lease.Epoch != 8 || shared.Lease.Flags != leaseParentKeySet {
		t.Fatalf("ordinary writer OPEN changed R: %+v", shared.Lease)
	}

	// Drain a real R-only invalidation if the companion hook is present. With
	// the pre-gate production checkpoint no notification arrives; cancellation
	// at cleanup joins this receiver without introducing a fake state helper.
	notifications := make(chan error, 1)
	notifyCtx, stopNotifications := context.WithCancel(holder.ctx)
	go func() {
		_, receiveErr := holder.client.WaitLeaseBreak(notifyCtx)
		notifications <- receiveErr
	}()
	t.Cleanup(func() { stopNotifications(); <-notifications })

	write := writer.header(wire.Write)
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: opened.Reply.ID, Data: []byte("new")})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := writer.client.Send(writer.ctx, []wire.Message{{Header: write, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-storage.entered:
	case <-writer.ctx.Done():
		t.Fatal(writer.ctx.Err())
	}
	pending, err := writer.client.Receive(writer.ctx)
	if err != nil || len(pending.Messages) != 1 {
		t.Fatalf("WRITE pending = %+v, error %v", pending, err)
	}
	header := pending.Messages[0].Header
	if header.MessageID != write.MessageID || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 {
		t.Fatalf("WRITE pending identity = %+v", header)
	}

	during := reader.create(t, leaseCreateRequest("file"), leaseV2(2, smb.LeaseRead))
	if during.Lease == nil || during.Lease.State != 0 || during.Reply.OplockLevel != 0 {
		// Continue through every release outcome even on the expected RED, so
		// the failing grant oracle never leaves the blocked adapter behind.
		t.Errorf("fresh R granted during actual WriteAt: lease %+v, oplock %#x", during.Lease, during.Reply.OplockLevel)
	}
	final := finishCreateMutationWrite(t, writer, write, header, unblock, outcome)
	want := smb.StatusSuccess
	switch outcome {
	case "error":
		want = smb.StatusIODeviceError
	case "cancel":
		want = smb.StatusCancelled
	}
	if final.Header.Status != want || final.Header.MessageID != write.MessageID || final.Header.Flags&wire.FlagAsync == 0 || final.Header.AsyncID != header.AsyncID || final.Header.Credit != 0 {
		t.Fatalf("WRITE final identity/status = %+v, want %#x", final.Header, want)
	}
	if outcome == "success" {
		written, decodeErr := wire.DecodeWriteResponse(final)
		if decodeErr != nil || written.Count != 3 {
			t.Fatalf("WRITE result = %+v, error %v", written, decodeErr)
		}
	}
	after := reader.create(t, leaseCreateRequest("file"), leaseV2(3, smb.LeaseRead))
	assertLeaseGrant(t, after, smb.LeaseRead)
	if after.Lease.Epoch != 8 || after.Lease.Flags != leaseParentKeySet {
		t.Fatalf("released mutation still withheld R: %+v", after.Lease)
	}
	checkCreateMutationWriteBytes(t, reader, after.Reply.ID, outcome)
	t.Logf("%s release completed; final async identity, restored R and actual bytes verified", outcome)
	reader.close(t, during.Reply.ID)
	reader.close(t, after.Reply.ID)
	writer.close(t, opened.Reply.ID)
}

// From create_mutation_gate_test.go.
func finishCreateMutationWrite(t *testing.T, writer *createLeaseClient, write, pending wire.Header, unblock func(), outcome string) wire.Message {
	t.Helper()
	if outcome == "cancel" {
		cancel := writer.header(wire.Cancel)
		cancel.MessageID, cancel.AsyncID = write.MessageID, pending.AsyncID
		cancel.Credit, cancel.CreditCharge, cancel.Flags = 0, 0, wire.FlagAsync
		cancel.TreeID = 0
		body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if sendErr := writer.client.Send(writer.ctx, []wire.Message{{Header: cancel, Body: body}}); sendErr != nil {
			t.Fatal(sendErr)
		}
	} else {
		unblock()
	}
	final, err := writer.receive(write.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	return final
}

// From create_mutation_gate_test.go.
func checkCreateMutationWriteBytes(t *testing.T, reader *createLeaseClient, id wire.FileID, outcome string) {
	t.Helper()
	header := reader.header(wire.Read)
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := reader.client.Send(reader.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	result, err := reader.receive(header.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "success" {
		if result.Header.Status != smb.StatusEndOfFile {
			t.Fatalf("failed/canceled WRITE changed bytes: %#x", result.Header.Status)
		}
		return
	}
	read, decodeErr := wire.DecodeReadResponse(result)
	if result.Header.Status != smb.StatusSuccess || decodeErr != nil || string(read.Data) != "new" {
		t.Fatalf("successful WRITE bytes = %+v, status %#x, error %v", read, result.Header.Status, decodeErr)
	}
}

// From create_review_test.go.
func checkCreateWriteThrough(t *testing.T, failBarrier bool) {
	t.Helper()
	barrier := &writeBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	var fail atomic.Bool
	fail.Store(failBarrier)
	fixture := newIOFixture(t, flushBarrier(func(ctx context.Context, full bool) error {
		if err := barrier.Commit(ctx, full); err != nil {
			return err
		}
		if fail.Swap(false) {
			return smb.ErrIO
		}
		return nil
	}))
	client := newReadWriteClient(t, fixture.adapter)
	unblock := sync.OnceFunc(func() { close(barrier.resume) })
	t.Cleanup(unblock)
	request := createRequest("flush-data", fileCreateDisposition)
	request.Options = 0x00000002 // FILE_WRITE_THROUGH in CREATE, not WRITE.
	opened := createdFile(t, client.create(t, request))
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: opened.ID, Data: []byte("durable"), Flags: 0})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.Write, body, 1)
	client.next++
	if sendErr := client.client.Send(client.ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	pending, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, pending.Messages[0], smb.StatusPending)
	select {
	case <-barrier.entered:
	case <-client.ctx.Done():
		t.Fatal(client.ctx.Err())
	}
	if barrier.full || fixture.store.puts.Load() == 0 {
		t.Fatal("CREATE write-through did not upload with SyncData")
	}
	assertCommittedFlushData(t, fixture, []byte("durable"))
	// ECHO is next while WRITE is still blocked in the metadata barrier.
	requireIOStatus(t, ioRoundTrip(client.ctx, t, client.client, sessionEcho(t, client.session, client.next)), smb.StatusSuccess)
	client.next++
	unblock()
	final, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := smb.StatusSuccess
	if failBarrier {
		want = smb.StatusIODeviceError
	}
	assertAsyncFinal(t, final.Messages[0], pending.Messages[0], want)
	if !failBarrier {
		written, err := wire.DecodeWriteResponse(final.Messages[0])
		if err != nil || written.Count != 7 {
			t.Fatalf("write-through final count: %+v, %v", written, err)
		}
	}
	requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
}

// From delete_drop_test.go.
type droppingDeletionPeer struct {
	peer *deletionPeer
	done chan struct{}
	err  error
}

// From delete_drop_test.go.
// This peer exposes transport cleanup completion without shutting down other
// connections. They must keep their handles while the deleting peer drops.
func newDroppingDeletionPeer(t *testing.T, server *Server) *droppingDeletionPeer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	dropping := &droppingDeletionPeer{done: make(chan struct{})}
	go func() {
		dropping.err = server.ServeConn(ctx, local)
		close(dropping.done)
	}()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		<-dropping.done
		if dropping.err != nil && !errors.Is(dropping.err, context.Canceled) {
			t.Error(dropping.err)
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	dropping.peer = &deletionPeer{ctx: ctx, client: client, session: session, next: session.NextMessageID}
	return dropping
}

// From delete_drop_test.go.
func (dropping *droppingDeletionPeer) drop(t *testing.T) {
	t.Helper()
	if err := dropping.peer.client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dropping.done:
		if dropping.err != nil {
			t.Fatal(dropping.err)
		}
	case <-dropping.peer.ctx.Done():
		t.Fatal(dropping.peer.ctx.Err())
	}
}

// From delete_e2e_test.go.
type deletionPeer struct {
	ctx     context.Context
	client  *smbtest.Client
	session smbtest.Session
	next    uint64
}

// From delete_e2e_test.go.
func newDeletionPeer(t *testing.T, server *Server) *deletionPeer {
	t.Helper()
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	return &deletionPeer{ctx: ctx, client: client, session: session, next: session.NextMessageID}
}

// From delete_e2e_test.go.
func (peer *deletionPeer) message(command wire.Command, body []byte) wire.Message {
	message := wire.Message{Header: wire.Header{Command: command, MessageID: peer.next, SessionID: peer.session.SessionID, TreeID: peer.session.TreeID, CreditCharge: 1, Credit: 16}, Body: body}
	peer.next++
	return message
}

// From delete_e2e_test.go.
func (peer *deletionPeer) open(t *testing.T, path string, access, disposition, options uint32, want smb.Status) wire.FileID {
	t.Helper()
	body, err := wire.EncodeCreateRequest(wire.CreateRequest{Name: path, DesiredAccess: access, Disposition: disposition, Options: options, ShareAccess: 7, ImpersonationLevel: 2})
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(peer.ctx, t, peer.client, peer.message(wire.Create, body))
	if response.Header.Status != want {
		t.Fatalf("CREATE %s: %#x, want %#x", path, response.Header.Status, want)
	}
	if want != smb.StatusSuccess {
		return wire.FileID{}
	}
	create, err := wire.DecodeCreateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return create.ID
}

// From delete_e2e_test.go.
func (peer *deletionPeer) close(t *testing.T, id wire.FileID) {
	t.Helper()
	peer.closeExpect(t, id, smb.StatusSuccess)
}

// From delete_e2e_test.go.
func (peer *deletionPeer) closeExpect(t *testing.T, id wire.FileID, want smb.Status) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(peer.ctx, t, peer.client, peer.message(wire.Close, body))[0]
	if response.Header.Status != want {
		t.Fatalf("CLOSE: %#x, want %#x", response.Header.Status, want)
	}
}

// From delete_e2e_test.go.
func (peer *deletionPeer) disposition(t *testing.T, id wire.FileID, pending bool, want smb.Status) {
	t.Helper()
	input, err := wire.EncodeFileDispositionInformation(wire.FileDispositionInformation{DeletePending: pending})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileDisposition), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(peer.ctx, t, peer.client, peer.message(wire.SetInfo, body))
	if response.Header.Status != want {
		t.Fatalf("SET_INFO disposition: %#x, want %#x", response.Header.Status, want)
	}
	if want == smb.StatusSuccess {
		if _, err := wire.DecodeSetInfoResponse(response); err != nil {
			t.Fatal(err)
		}
	}
}

// From delete_e2e_test.go.
func deletionServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// From delete_e2e_test.go.
func seedDeletionData(t *testing.T, storage smb.Storage, path, data string) smb.Resolved {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = storage.Create(t.Context(), resolved.Name, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := storage.WriteAt(t.Context(), handle, []byte(data), 0)
	closeErr := storage.Close(t.Context(), handle)
	if err := errors.Join(writeErr, closeErr); err != nil || n != len(data) {
		t.Fatalf("seed %s = %d, %v", path, n, err)
	}
	return resolved
}

// From delete_e2e_test.go.
func requireDeletionData(t *testing.T, storage smb.Storage, path, want string) {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil || !resolved.Exists {
		t.Fatalf("lookup %s = %+v, %v", path, resolved, err)
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len(want)+1)
	n, readErr := storage.ReadAt(t.Context(), handle, data, 0)
	closeErr := storage.Close(t.Context(), handle)
	if closeErr != nil || !errors.Is(readErr, io.EOF) || !bytes.Equal(data[:n], []byte(want)) {
		t.Fatalf("read %s = %q, %v, close %v; want %q", path, data[:n], readErr, closeErr, want)
	}
}

// From delete_e2e_test.go.
func requireDeletionMissing(t *testing.T, storage smb.Storage, path string) {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil || resolved.Exists {
		t.Fatalf("deleted %s = %+v, %v", path, resolved, err)
	}
}

// From delete_e2e_test.go.
func checkStreamDeletion(t *testing.T, ending, other string) {
	t.Helper()
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "keep base data")
	seedDeletionData(t, storage, "data:AFP_Resource:$DATA", "delete resource")
	seedDeletionData(t, storage, "data:keep:$DATA", "keep other stream")
	peer := newDeletionPeer(t, server)
	var held wire.FileID
	if other != "none" {
		path := "data"
		if other == "same stream" {
			path = "data:AFP_Resource:$DATA"
		}
		held = peer.open(t, path, fileReadData, fileOpen, 0, smb.StatusSuccess)
	}
	deleted := peer.open(t, "data:AFP_Resource:$DATA", fileReadData|fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	if ending == "close" {
		peer.close(t, deleted)
		if other == "same stream" {
			requireDeletionData(t, storage, "data:AFP_Resource:$DATA", "delete resource")
			peer.open(t, "data:AFP_Resource:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
			base := peer.open(t, "data", fileReadData, fileOpen, 0, smb.StatusSuccess)
			peer.close(t, base)
		}
		if held != (wire.FileID{}) {
			if other == "base" {
				requireDeletionMissing(t, storage, "data:AFP_Resource:$DATA")
			}
			peer.close(t, held)
		}
	} else {
		endDeletionSession(t, server, peer, ending)
	}
	requireDeletionMissing(t, storage, "data:AFP_Resource:$DATA")
	requireDeletionData(t, storage, "data", "keep base data")
	requireDeletionData(t, storage, "data:keep:$DATA", "keep other stream")
}

// From delete_e2e_test.go.
func endDeletionSession(t *testing.T, server *Server, peer *deletionPeer, ending string) {
	t.Helper()
	switch ending {
	case "drop":
		if err := peer.client.Close(); err != nil {
			t.Fatal(err)
		}
		server.workers.Wait()
	case "shutdown":
		if err := server.Shutdown(peer.ctx); err != nil {
			t.Fatal(err)
		}
	case "logoff", "tree disconnect":
		command := wire.Logoff
		if ending == "tree disconnect" {
			command = wire.TreeDisconnect
		}
		message := treeRequest(t, peer.session, peer.next, command)
		peer.next++
		response := exchange(peer.ctx, t, peer.client, message)[0]
		if response.Header.Status != smb.StatusSuccess {
			t.Fatalf("%s: %#x", ending, response.Header.Status)
		}
	default:
		t.Fatalf("unknown ending %q", ending)
	}
}

// From delete_e2e_test.go.
// Pausing Close exposes the bulk-close window after Table.CloseSession has
// removed the last open but before cleanup takes the deletion's parent guard.
type pausedDeletionStorage struct {
	smb.Storage
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

// From delete_e2e_test.go.
func (storage *pausedDeletionStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.once.Do(func() {
		close(storage.entered)
		<-storage.resume
	})
	return storage.Storage.Close(ctx, handle)
}

// From delete_failed_create_test.go.
type failedCreateCleanupStorage struct {
	smb.Storage
	closeErr  error
	removeErr error
}

// From delete_failed_create_test.go.
func (storage *failedCreateCleanupStorage) Close(ctx context.Context, handle smb.Handle) error {
	return errors.Join(storage.Storage.Close(ctx, handle), storage.closeErr)
}

// From delete_failed_create_test.go.
func (storage *failedCreateCleanupStorage) Remove(ctx context.Context, name smb.Name, inode smb.Inode) error {
	if storage.removeErr != nil {
		return storage.removeErr
	}
	return storage.Storage.Remove(ctx, name, inode)
}

// From delete_failed_create_test.go.
func checkFailedCreateDeletion(t *testing.T, outcome string) {
	t.Helper()
	storage := newFilesMetaStorage(t)
	selected := seedDeletionData(t, storage, "data", "base")
	seedDeletionData(t, storage, "data:stream:$DATA", "stream")
	wrapped := &failedCreateCleanupStorage{Storage: storage}
	options := testOptions(t)
	options.Storage = wrapped
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	peer := newDeletionPeer(t, server)
	id := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	request := deletionRequest(server, peer)
	open, status := request.Opens.Find(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, request.Binding())
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	ctx := t.Context()
	var want error
	switch outcome {
	case "close failure":
		wrapped.closeErr, want = smb.ErrIO, smb.ErrIO
	case "remove failure":
		wrapped.removeErr, want = smb.ErrIO, smb.ErrIO
	case "changed inode":
		replacement := seedDeletionData(t, storage, "replacement", "new base")
		seedDeletionData(t, storage, "replacement:stream:$DATA", "new stream")
		if renameErr := storage.Rename(ctx, smb.RenameRequest{Source: replacement.Name, SourceInode: replacement.Object.Inode, Destination: selected.Name, DestinationInode: selected.Object.Inode, Replace: true}); renameErr != nil {
			t.Fatal(renameErr)
		}
		want = smb.ErrIdentityChanged
	case "remove cancelled":
		wrapped.removeErr, want = context.Canceled, context.Canceled
	}
	// CREATE still owns the parent when response encoding fails. Exercise its
	// direct action consumer, not the bulk cleanup path that reacquires guards.
	unlock, err := lockParent(t.Context(), request, selected.Name.Parent)
	if err != nil {
		t.Fatal(err)
	}
	err = closeFailedCreate(ctx, request, open)
	unlock()
	if want == nil && err != nil || want != nil && !errors.Is(err, want) {
		t.Fatalf("failed CREATE cleanup = %v, want %v", err, want)
	}
	wrapped.closeErr = nil
	switch outcome {
	case "success", "close failure":
		requireDeletionMissing(t, storage, "data:stream:$DATA")
		requireDeletionData(t, storage, "data", "base")
	case "changed inode":
		requireDeletionData(t, storage, "data", "new base")
		requireDeletionData(t, storage, "data:stream:$DATA", "new stream")
	default:
		requireDeletionData(t, storage, "data", "base")
		requireDeletionData(t, storage, "data:stream:$DATA", "stream")
	}
	reservation, status := request.Opens.Reserve(state.OpenRequest{Object: open.Object, Binding: request.Binding(), GrantedAccess: fileDelete, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatalf("failed CREATE retained delete-pending: %#x", status)
	}
	if status := request.Opens.Abort(reservation); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	id = peer.open(t, "data:stream:$DATA", fileReadData, fileOpenIf, 0, smb.StatusSuccess)
	peer.close(t, id)
}

// From delete_failed_create_test.go.
func deletionRequest(server *Server, peer *deletionPeer) RequestContext {
	return RequestContext{
		server: server, Storage: server.options.Storage, Opens: server.options.State,
		Session: Session{SessionID: peer.session.SessionID}, Tree: Tree{TreeID: peer.session.TreeID},
	}
}

// From delete_hardlink_test.go.
func checkSeededHardlinkDeletion(t *testing.T, ending string) {
	t.Helper()
	storage, metadata := newFilesMetaStorageWithMetadata(t, 0)
	first := seedDeletionData(t, storage, "a", "keep both links")
	var attr meta.Attr
	if errno := metadata.Link(meta.WrapWithoutCancel(t.Context(), 0, smbfs.UID, []uint32{smbfs.GID}), meta.Ino(first.Object.Inode), meta.RootInode, "b", &attr); errno != 0 {
		t.Fatal(errno)
	}
	second, err := storage.Lookup(t.Context(), "b")
	if err != nil || !second.Exists || second.Object != first.Object || attr.Nlink != 2 {
		t.Fatalf("seeded second link = %+v, nlink %d, %v", second, attr.Nlink, err)
	}
	if path, pathErr := storage.PathOf(t.Context(), first.Object.Inode); !errors.Is(pathErr, smb.ErrNameNotFound) {
		t.Fatalf("PathOf accepted multiple names: %q, %v", path, pathErr)
	}
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	peer := newDeletionPeer(t, server)
	deleted := peer.open(t, "a", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	last := peer.open(t, "b", fileReadData, fileOpen, 0, smb.StatusSuccess)
	input, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: "third"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: deleted, InfoType: wire.InfoFile, InfoClass: 11, Input: input}) // FileLinkInformation.
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(peer.ctx, t, peer.client, peer.message(wire.SetInfo, body))[0]
	if response.Header.Status != smb.StatusNotSupported {
		t.Fatalf("SMB FileLink = %#x", response.Header.Status)
	}
	peer.close(t, deleted)
	peer.open(t, "b", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	if ending == "close" {
		peer.close(t, last)
	} else {
		endDeletionSession(t, server, peer, "logoff")
	}
	requireDeletionData(t, storage, "a", "keep both links")
	requireDeletionData(t, storage, "b", "keep both links")
	requireDeletionMissing(t, storage, "third")
	other := newDeletionPeer(t, server)
	id := other.open(t, "b", fileReadData, fileOpen, 0, smb.StatusSuccess)
	other.close(t, id)
}

// From delete_identity_test.go.
// A storage-side rename can change a name even while the server holds its
// namespace guard. The real adapter must check the inode again inside Remove.
type replacedDeletionStorage struct {
	smb.Storage
	replacement smb.RenameRequest
}

// From delete_identity_test.go.
func (storage *replacedDeletionStorage) Remove(ctx context.Context, name smb.Name, expect smb.Inode) error {
	if err := storage.Rename(ctx, storage.replacement); err != nil {
		return err
	}
	return storage.Storage.Remove(ctx, name, expect)
}

// From delete_retry_test.go.
// CLOSE resolves PathOf once before table removal and again during cleanup.
// Pause the second result before cleanup can acquire its parent guard.
type pausedDeletionPathStorage struct {
	smb.Storage
	entered   chan struct{}
	resume    chan struct{}
	remaining atomic.Int32
}

// From delete_retry_test.go.
func (storage *pausedDeletionPathStorage) PathOf(ctx context.Context, inode smb.Inode) (string, error) {
	path, err := storage.Storage.PathOf(ctx, inode)
	if storage.remaining.Add(-1) == 0 {
		close(storage.entered)
		select {
		case <-storage.resume:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return path, err
}

// From delete_retry_test.go.
func checkDeletionPathRace(t *testing.T, stream, outcome string) {
	t.Helper()
	storage := newFilesMetaStorage(t)
	source := seedDeletionData(t, storage, "selected", "old base")
	if stream != "" {
		seedDeletionData(t, storage, "selected"+stream, "old stream")
		seedDeletionData(t, storage, "selected:other:$DATA", "keep stream")
	}
	paused := &pausedDeletionPathStorage{Storage: storage, entered: make(chan struct{}), resume: make(chan struct{})}
	var once sync.Once
	resume := func() { once.Do(func() { close(paused.resume) }) }
	defer resume()
	options := testOptions(t)
	options.Storage = paused
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	peer := newDeletionPeer(t, server)
	var held wire.FileID
	if stream != "" {
		held = peer.open(t, "selected", fileReadData, fileOpen, 0, smb.StatusSuccess)
	}
	id := peer.open(t, "selected"+stream, fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	paused.remaining.Store(2)
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := peer.client.Send(peer.ctx, []wire.Message{peer.message(wire.Close, body)}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-paused.entered:
	case <-peer.ctx.Done():
		t.Fatal(peer.ctx.Err())
	}
	if outcome == "gone" {
		if removeErr := storage.Remove(t.Context(), source.Name, source.Object.Inode); removeErr != nil {
			t.Fatal(removeErr)
		}
	} else {
		destination, lookupErr := storage.Lookup(t.Context(), "moved")
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if renameErr := storage.Rename(t.Context(), smb.RenameRequest{Source: source.Name, SourceInode: source.Object.Inode, Destination: destination.Name}); renameErr != nil {
			t.Fatal(renameErr)
		}
	}
	if outcome != "renamed" {
		seedDeletionData(t, storage, "selected", "replacement base")
		if stream != "" {
			seedDeletionData(t, storage, "selected"+stream, "replacement stream")
		}
	}
	resume()
	response, err := peer.client.Receive(peer.ctx)
	if err != nil || response.Messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE after %s = %+v, %v", outcome, response, err)
	}
	if outcome != "gone" {
		requireDeletionMissing(t, storage, "moved"+stream)
		if stream != "" {
			requireDeletionData(t, storage, "moved", "old base")
			requireDeletionData(t, storage, "moved:other:$DATA", "keep stream")
		}
	}
	if outcome != "renamed" {
		requireDeletionData(t, storage, "selected", "replacement base")
		if stream != "" {
			requireDeletionData(t, storage, "selected"+stream, "replacement stream")
		}
	}
	if held != (wire.FileID{}) && outcome != "gone" {
		peer.close(t, held)
	}
}

// From delete_test.go.
type deletionStorage struct {
	pathErr   error
	lookupErr error
	cleanupStorage
	name    smb.Name
	inode   smb.Inode
	changed bool
}

// From delete_test.go.
func (storage *deletionStorage) Remove(_ context.Context, name smb.Name, inode smb.Inode) error {
	storage.name, storage.inode = name, inode
	return storage.removeErr
}

// From delete_test.go.
func (storage *deletionStorage) PathOf(context.Context, smb.Inode) (string, error) {
	err := storage.pathErr
	if storage.changed {
		storage.pathErr = smb.ErrNameNotFound
	}
	return "renamed", err
}

// From delete_test.go.
func (storage *deletionStorage) Lookup(context.Context, string) (smb.Resolved, error) {
	inode := smb.Inode(2)
	if storage.changed {
		inode = 3
	}
	return smb.Resolved{Exists: true, Object: smb.ObjectKey{Inode: inode}, Name: smb.Name{Parent: 1, Base: "renamed"}}, storage.lookupErr
}

// From delete_test.go.
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
	if (err == nil) != (outcome == "success" || outcome == "gone inode" || outcome == "changed inode") {
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

// From delete_test.go.
func cleanupOutcome(outcome string) *deletionStorage {
	storage := &deletionStorage{}
	switch outcome {
	case "remove failure":
		storage.removeErr = smb.ErrIO
	case "cancelled":
		storage.removeErr = context.Canceled
	case "gone inode":
		storage.pathErr = smb.ErrNameNotFound
	case "path failure":
		storage.pathErr = smb.ErrIO
	case "lookup failure":
		storage.lookupErr = smb.ErrIO
	case "changed inode":
		storage.changed = true
	}
	return storage
}

// From durable_lock_sequence_test.go.
func (fixture *reconnectFixture) lockSequence(t *testing.T, client *smbtest.Client, session *smbtest.Session, id wire.FileID, sequence uint32, elements ...wire.LockElement) smb.Status {
	t.Helper()
	body, err := wire.EncodeLockRequest(wire.LockRequest{ID: id, Sequence: sequence, Elements: elements})
	if err != nil {
		t.Fatal(err)
	}
	header := reconnectHeader(session, wire.Lock)
	if sendErr := client.Send(fixture.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	response, err := client.Receive(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatal("expected one LOCK reply")
	}
	message := response.Messages[0]
	if message.Header.MessageID != header.MessageID || message.Header.Command != wire.Lock || message.Header.Status == smb.StatusPending || message.Header.Flags&wire.FlagAsync != 0 {
		t.Fatalf("wrong or asynchronous LOCK reply: %+v", message.Header)
	}
	if message.Header.Status == smb.StatusSuccess {
		if _, decodeErr := wire.DecodeLockResponse(message); decodeErr != nil {
			t.Fatal(decodeErr)
		}
	}
	return message.Header.Status
}

// From durable_lock_sequence_test.go.
func requireLockSequenceStatus(t *testing.T, got, want smb.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("LOCK status = %#x, want %#x", got, want)
	}
}

// From durable_reconnect_netfault_test.go.
func reconnectHeader(session *smbtest.Session, command wire.Command) wire.Header {
	header := wire.Header{Command: command, MessageID: session.NextMessageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 32}
	session.NextMessageID++
	return header
}

// From durable_reconnect_netfault_test.go.
func reconnectReceive(ctx context.Context, t *testing.T, client *smbtest.Client, header wire.Header) wire.Message {
	t.Helper()
	for {
		reply, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Messages) != 1 {
			t.Fatal("expected one reconnect test reply")
		}
		message := reply.Messages[0]
		if message.Header.MessageID != header.MessageID || message.Header.Command != header.Command || message.Header.SessionID != header.SessionID {
			t.Fatalf("wrong reconnect reply: %+v", message.Header)
		}
		if message.Header.Status != smb.StatusPending {
			return message
		}
	}
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) create(t *testing.T, client *smbtest.Client, session *smbtest.Session, options smbtest.CreateOptions) wire.Message {
	t.Helper()
	header := reconnectHeader(session, wire.Create)
	if err := client.SendCreate(fixture.ctx, header, options); err != nil {
		t.Fatal(err)
	}
	return reconnectReceive(fixture.ctx, t, client, header)
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) durable(t *testing.T, client *smbtest.Client, session *smbtest.Session, leaseState, sharing, options uint32) smbtest.RetainedOpen {
	t.Helper()
	create := wire.CreateRequest{Name: "band", DesiredAccess: fileAllAccess, ShareAccess: sharing, Disposition: fileOpenIf, Options: options}
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{74}, State: leaseState}
	request := wire.DurableRequest{CreateGUID: [16]byte{75}, Timeout: 120000}
	message := fixture.create(t, client, session, smbtest.CreateOptions{Request: create, Lease: &lease, Durable: &request})
	result, err := smbtest.DecodeCreateReply(message)
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease == nil || result.Lease.State != leaseState || result.Durable == nil || result.Durable.Timeout != request.Timeout {
		t.Fatalf("missing lease or durable grant: %+v", result)
	}
	return smbtest.RetainedOpen{Request: create, ID: result.Reply.ID, Lease: *result.Lease, CreateGUID: request.CreateGUID, ClientGUID: session.ClientGUID}
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) cut(t *testing.T, transport *reconnectTransport) {
	t.Helper()
	if err := transport.proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	awaitReconnectEvent(fixture.ctx, t, transport.done)
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) reopen(t *testing.T, previous smbtest.Session, open smbtest.RetainedOpen) (*smbtest.Client, smbtest.Session, wire.FileID) {
	t.Helper()
	transport := fixture.transport(t)
	client, session, results, err := smbtest.Reconnect(fixture.ctx, transport.conn, previous, fixture.login, []smbtest.RetainedOpen{open})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if len(results) != 1 || results[0].Reply.ID.Persistent != open.ID.Persistent || results[0].Reply.ID.Volatile == open.ID.Volatile || session.SessionID == previous.SessionID {
		t.Fatalf("reconnect did not retain persistent identity with a new binding: %+v, %+v", session, results)
	}
	return client, session, results[0].Reply.ID
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) refused(t *testing.T, previous smbtest.Session, open smbtest.RetainedOpen) {
	t.Helper()
	transport := fixture.transport(t)
	client, err := smbtest.NewClient(transport.conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	options := fixture.login
	options.PreviousSessionID = previous.SessionID
	session, err := client.Login(fixture.ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	message := fixture.create(t, client, &session, smbtest.CreateOptions{
		Request: open.Request, Lease: &open.Lease,
		Reconnect: &wire.DurableReconnect{ID: open.ID, CreateGUID: open.CreateGUID},
	})
	if message.Header.Status != smb.StatusObjectNameNotFound {
		t.Fatalf("DH2C refusal = %#x, want OBJECT_NAME_NOT_FOUND", message.Header.Status)
	}
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) lock(t *testing.T, client *smbtest.Client, session *smbtest.Session, id wire.FileID, flags uint32) smb.Status {
	t.Helper()
	body, err := wire.EncodeLockRequest(wire.LockRequest{ID: id, Elements: []wire.LockElement{{Offset: 0, Length: 17, Flags: flags}}})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: reconnectHeader(session, wire.Lock), Body: body}
	return ioRoundTrip(fixture.ctx, t, client, message).Header.Status
}

// From durable_reconnect_netfault_test.go.
func (fixture *reconnectFixture) io(t *testing.T, client *smbtest.Client, session *smbtest.Session, command wire.Command, id wire.FileID, data []byte, offset uint64) wire.Message {
	t.Helper()
	message := reconnectIO(t, session, command, id, data, offset)
	return ioRoundTrip(fixture.ctx, t, client, message)
}

// From durable_reconnect_netfault_test.go.
func requireReconnectStatus(t *testing.T, message wire.Message, status smb.Status) {
	t.Helper()
	if message.Header.Status != status {
		t.Fatalf("%d status = %#x, want %#x", message.Header.Command, message.Header.Status, status)
	}
}

// From durable_reconnect_netfault_test.go.
func requireReconnectData(t *testing.T, message wire.Message, want string) {
	t.Helper()
	requireReconnectStatus(t, message, smb.StatusSuccess)
	response, err := wire.DecodeReadResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Data) != want {
		t.Fatalf("reconnected data = %q, want %q", response.Data, want)
	}
}

// From durable_reconnect_netfault_test.go.
func checkDurableReconnectIO(t *testing.T, cipher uint16, command wire.Command) {
	t.Helper()
	fixture := newReconnectFixture(t, cipher)
	client, session, transport := fixture.client(t, fixture.login.ClientGUID)
	open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 1, 0)
	acknowledged := "acknowledged data"
	requireReconnectStatus(t, fixture.io(t, client, &session, wire.Write, open.ID, []byte(acknowledged), 0), smb.StatusSuccess)
	if status := fixture.lock(t, client, &session, open.ID, 2); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	peer, peerSession, _ := fixture.client(t, [16]byte{76})
	peerOpen, err := smbtest.DecodeCreateReply(fixture.create(t, peer, &peerSession, smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: "band", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen},
	}))
	if err != nil {
		t.Fatal(err)
	}
	gate := fixture.storage.arm(command)
	message := reconnectIO(t, &session, command, open.ID, []byte(acknowledged), 0)
	if sendErr := client.Send(fixture.ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	awaitReconnectEvent(fixture.ctx, t, gate.entered)
	fixture.cut(t, transport)
	awaitReconnectEvent(fixture.ctx, t, gate.canceled)
	for {
		reply, receiveErr := client.Receive(fixture.ctx)
		if receiveErr != nil {
			break
		}
		if len(reply.Messages) != 1 || reply.Messages[0].Header.Status != smb.StatusPending {
			t.Fatalf("old request completed after cut: %+v", reply)
		}
	}
	selected, err := fixture.storage.Lookup(fixture.ctx, "band")
	if err != nil {
		t.Fatal(err)
	}
	// A raw CREATE may break H and close a fully detached open. Check sharing
	// before that exception, without starting a break or mutating storage.
	token, status := fixture.server.options.State.Reserve(state.OpenRequest{
		Object: selected.Object, Binding: state.Binding{SessionID: peerSession.SessionID, TreeID: peerSession.TreeID},
		GrantedAccess: fileWriteData, Sharing: 7,
	})
	if status == smb.StatusSuccess {
		if abortStatus := fixture.server.options.State.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
	}
	if status != smb.StatusSharingViolation {
		t.Fatalf("detached sharing = %#x", status)
	}
	if status := fixture.lock(t, peer, &peerSession, peerOpen.Reply.ID, 2); status != smb.StatusLockNotGranted {
		t.Fatalf("detached range = %#x", status)
	}
	fixture.clock.advance(30 * time.Second)
	resumed, resumedSession, id := fixture.reopen(t, session, open)
	requireReconnectStatus(t, fixture.io(t, resumed, &resumedSession, wire.Read, open.ID, []byte(acknowledged), 0), smb.StatusFileClosed)
	requireReconnectData(t, fixture.io(t, resumed, &resumedSession, wire.Read, id, []byte(acknowledged), 0), acknowledged)
	if status := fixture.lock(t, peer, &peerSession, peerOpen.Reply.ID, 2); status != smb.StatusLockNotGranted {
		t.Fatalf("reattached range = %#x", status)
	}
	requireReconnectStatus(t, fixture.io(t, resumed, &resumedSession, wire.Write, id, []byte(" continued"), 17), smb.StatusSuccess)
	requireReconnectStatus(t, fixture.io(t, resumed, &resumedSession, wire.Flush, id, nil, 0), smb.StatusSuccess)
	want := acknowledged + " continued"
	requireReconnectData(t, fixture.io(t, resumed, &resumedSession, wire.Read, id, []byte(want), 0), want)
	if status := fixture.lock(t, resumed, &resumedSession, id, 4); status != smb.StatusSuccess {
		t.Fatalf("retained owner's unlock = %#x", status)
	}
	if status := fixture.lock(t, peer, &peerSession, peerOpen.Reply.ID, 2); status != smb.StatusSuccess {
		t.Fatalf("peer lock after unlock = %#x", status)
	}
}

// From durable_shared_lease_test.go.
func createSharedDurable(t *testing.T, client *createLeaseClient, lease *wire.LeaseContext) smbtest.CreateResult {
	t.Helper()
	header := client.header(wire.Create)
	if err := client.client.SendCreate(client.ctx, header, smbtest.CreateOptions{
		Request: leaseCreateRequest("file"), Lease: lease, Durable: &wire.DurableRequest{CreateGUID: [16]byte{5}},
	}); err != nil {
		t.Fatal(err)
	}
	return client.created(t, header.MessageID)
}

// From durable_test.go.
func durableCreateOptions() smbtest.CreateOptions {
	return smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: "durable", DesiredAccess: 0xc0000000, ShareAccess: 7, Disposition: fileOpenIf, Options: fileNonDirectoryFile},
		Lease:   &wire.LeaseContext{Version: 2, Key: [16]byte{4}, State: smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle},
		Durable: &wire.DurableRequest{CreateGUID: [16]byte{5}},
	}
}

// From durable_test.go.
func durableExchange(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, options smbtest.CreateOptions) wire.Message {
	t.Helper()
	header := wire.Header{MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}
	if err := client.SendCreate(ctx, header, options); err != nil {
		t.Fatal(err)
	}
	for {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 || response.Messages[0].Header.MessageID != id {
			t.Fatalf("unexpected CREATE reply: %+v", response.Messages)
		}
		if response.Messages[0].Header.Status != smb.StatusPending {
			return response.Messages[0]
		}
	}
}

// From durable_test.go.
func durableResult(t *testing.T, message wire.Message) smbtest.CreateResult {
	t.Helper()
	result, err := smbtest.DecodeCreateReply(message)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// From durable_test.go.
func durableOpen(t *testing.T, server *Server, session smbtest.Session, id wire.FileID) state.Open {
	t.Helper()
	open, status := server.options.State.Find(state.FileID(id), state.Binding{SessionID: session.SessionID, TreeID: session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("find durable open = %#x", status)
	}
	return open
}

// From files_meta_cleanup_test.go.
type filesMetaCleanupRecorder struct {
	*testing.T
	cleanups []func()
	reported []string
}

// From files_meta_cleanup_test.go.
func (test *filesMetaCleanupRecorder) Cleanup(cleanup func()) {
	test.cleanups = append(test.cleanups, cleanup)
}

// From files_meta_cleanup_test.go.
func (test *filesMetaCleanupRecorder) Error(args ...any) {
	test.reported = append(test.reported, fmt.Sprint(args...))
}

// From files_meta_cleanup_test.go.
func (test *filesMetaCleanupRecorder) finish() {
	for len(test.cleanups) > 0 {
		last := len(test.cleanups) - 1
		cleanup := test.cleanups[last]
		test.cleanups = test.cleanups[:last]
		cleanup()
	}
}

// From files_meta_cleanup_test.go.
type filesMetaCloseFailure struct {
	smb.Storage
	err error
}

// From files_meta_cleanup_test.go.
func (storage *filesMetaCloseFailure) Close(ctx context.Context, handle smb.Handle) error {
	return errors.Join(storage.Storage.Close(ctx, handle), storage.err)
}

// From files_meta_fixture_test.go.
// newFilesMetaStorage uses file-backed JuiceFS data and SQLite metadata.
// Register server cleanup after this helper so opens close before storage.
func newFilesMetaStorage(t *testing.T) *smbfs.FS {
	t.Helper()
	return newFilesMetaStorageWithCapacity(t, 0)
}

// From files_meta_fixture_test.go.
func newFilesMetaStorageWithCapacity(t *testing.T, capacity uint64) *smbfs.FS {
	t.Helper()
	storage, _ := newFilesMetaStorageWithMetadata(t, capacity)
	return storage
}

// From files_meta_fixture_test.go.
func newFilesMetaStorageWithMetadata(t *testing.T, capacity uint64) (*smbfs.FS, meta.Meta) {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	conf.Retries = 0
	database := filepath.Join(dir, "meta.db")
	metadata, err := meta.NewSQLite(database, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := metadata.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	format := meta.Format{Name: "files-meta-test", UUID: "files-meta-fixture", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true, Capacity: capacity}
	if err = metadata.Init(&format, true); err != nil {
		t.Fatal(err)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if errno := metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); errno != 0 {
		t.Fatal(errno)
	}
	if err = metadata.NewSession(true); err != nil {
		t.Fatal(err)
	}
	chunks := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: 5 * time.Second, PutTimeout: time.Second}
	store := chunk.NewCachedStore(blob, chunks, nil)
	config := &vfs.Config{Meta: conf, Format: format, Chunk: &chunks}
	filesystem, err := jfs.NewFileSystem(config, metadata, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := filesystem.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	barrier, err := smbfs.NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := smbfs.New(smbfs.Options{Filesystem: filesystem, Barrier: barrier, MetadataPath: database, Config: config, Store: store, Capacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return storage, metadata
}

// From files_meta_fixture_test.go.
type filesMetaClientTest interface {
	Helper()
	Context() context.Context
	Cleanup(func())
	Fatal(...any)
	Error(...any)
}

// From files_meta_fixture_test.go.
// newFilesMetaClient drives the public connection entry point with raw SMB.
func newFilesMetaClient(t filesMetaClientTest, server *Server) (*smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case serveErr := <-done:
			if unexpectedErr := filesMetaServeError(serveErr); unexpectedErr != nil {
				t.Error(unexpectedErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return client, ctx, session
}

// From files_meta_fixture_test.go.
// Filter only the plain outcomes of closing our net.Pipe and canceling its
// context. Recurse into joins, not wrappers: a wrapped storage cleanup error
// remains observable even when it wraps one of these same sentinels.
func filesMetaServeError(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result error
		for _, part := range joined.Unwrap() {
			result = errors.Join(result, filesMetaServeError(part))
		}
		return result
	}
	if _, wrapped := err.(interface{ Unwrap() error }); wrapped {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// From files_meta_fixture_test.go.
func insertFilesMetaOpen(t *testing.T, server *Server, session smbtest.Session, path string, grantedAccess uint32) state.Open {
	t.Helper()
	storage := server.options.Storage
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		resolved, err = storage.Create(t.Context(), resolved.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	reservation, status := server.options.State.Reserve(state.OpenRequest{
		Object: resolved.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID},
		User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: state.GUID{2},
		GrantedAccess: grantedAccess, Sharing: 7,
	})
	if status != smb.StatusSuccess {
		if closeErr := storage.Close(context.WithoutCancel(t.Context()), handle); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, state.Grant{Handle: handle, Directory: resolved.Attr.Kind == smb.KindDirectory})
	if status != smb.StatusSuccess {
		if abortStatus := server.options.State.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		if closeErr := storage.Close(context.WithoutCancel(t.Context()), handle); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(status)
	}
	return open
}

// From flush_test.go.
type flushBarrier func(context.Context, bool) error

// From flush_test.go.
func (barrier flushBarrier) Commit(ctx context.Context, full bool) error {
	return barrier(ctx, full)
}

// From flush_test.go.
func flushMessage(t *testing.T, session smbtest.Session, id uint64, file wire.FileID, reserved uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeFlushRequest(wire.FlushRequest{ID: file, Reserved1: reserved})
	if err != nil {
		t.Fatal(err)
	}
	return ioMessage(session, id, wire.Flush, body, 1)
}

// From flush_test.go.
func checkFlushBarrier(t *testing.T, reserved uint16) {
	t.Helper()
	started := make(chan bool, 1)
	resume := make(chan struct{})
	barrier := flushBarrier(func(ctx context.Context, full bool) error {
		select {
		case started <- full:
		default:
		}
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	fixture := newIOFixture(t, barrier)
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	// Always unblock the barrier before connection cleanup, including failures.
	unblock := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(unblock)
	writer := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID, wire.CreateRequest{
		Name: "flush-data", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileCreateDisposition,
	}))
	other := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID+1, wire.CreateRequest{
		Name: "flush-data", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen,
	}))
	session.NextMessageID += 2
	payload := []byte("cross-handle durable bytes")
	writeForFlush(ctx, t, client, session, writer.ID, payload)
	message := flushMessage(t, session, session.NextMessageID+1, other.ID, reserved)
	if sendErr := client.Send(ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case full := <-started:
		if full != (reserved == 0xffff) {
			t.Fatalf("full barrier = %v", full)
		}
	case <-ctx.Done():
		t.Fatal("metadata barrier not reached")
	}
	if fixture.store.puts.Load() == 0 {
		t.Fatal("metadata barrier ran before upload")
	}
	assertCommittedFlushData(t, fixture, payload)
	pending := receiveFlushReply(ctx, t, client)
	if pending.Header.Status != smb.StatusPending {
		t.Fatalf("reply before metadata barrier: %+v", pending.Header)
	}
	// ECHO must be the next reply while the flush barrier remains blocked.
	echoReply := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+2))[0]
	if echoReply.Header.Command != wire.Echo || echoReply.Header.Status != smb.StatusSuccess {
		t.Fatalf("flush completed before barrier: %+v", echoReply.Header)
	}
	unblock()
	final := receiveFlushReply(ctx, t, client)
	if final.Header.Status != smb.StatusSuccess || final.Header.MessageID != message.Header.MessageID {
		t.Fatal(final.Header)
	}
	if _, decodeErr := wire.DecodeFlushResponse(final); decodeErr != nil {
		t.Fatal(decodeErr)
	}
}

// From flush_test.go.
func writeForFlush(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id wire.FileID, payload []byte) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(ctx, t, client, ioMessage(session, session.NextMessageID, wire.Write, body, 1))
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	written, err := wire.DecodeWriteResponse(response)
	if err != nil || uint64(written.Count) != uint64(len(payload)) {
		t.Fatalf("write reply = %+v, %v", written, err)
	}
}

// From flush_test.go.
func assertCommittedFlushData(t *testing.T, fixture *ioFixture, payload []byte) {
	t.Helper()
	// A native reader uses committed slices, not the adapter's buffered writer.
	file, eno := fixture.native.Open(meta.Background(), "/flush-data", vfs.MODE_MASK_R)
	if eno != 0 {
		t.Fatal(eno)
	}
	data := make([]byte, len(payload))
	n, err := file.Pread(meta.Background(), data, 0)
	closeErr := file.Close(meta.Background())
	if err != nil || closeErr != 0 || n != len(payload) || string(data) != string(payload) {
		t.Fatalf("committed bytes = %q (%d), read %v, close %v", data, n, err, closeErr)
	}
}

// From flush_test.go.
func receiveFlushReply(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 || response.Messages[0].Header.Command != wire.Flush {
		t.Fatalf("not one FLUSH reply: %+v", response.Messages)
	}
	return response.Messages[0]
}

// From fs_info_test.go.
// This wrapper injects only a StatFS fault. All object operations use the real adapter.
type statFSFaultStorage struct {
	smb.Storage
	err error
}

// From fs_info_test.go.
func (storage statFSFaultStorage) StatFS(context.Context) (smb.Space, error) {
	// Nonzero data on failure must never reach the reply.
	return smb.Space{Capacity: 1 << 40, Free: 1 << 39, Available: 1 << 38}, storage.err
}

// From fs_info_wire_test.go.
// newQueryInfoFixtureWithStorage keeps real object storage while allowing
// configured capacity and a fault injected at the StatFS seam.
func newQueryInfoFixtureWithStorage(t *testing.T, storage smb.Storage) *queryInfoFixture {
	t.Helper()
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case serveErr := <-done:
			if serveErr != nil && ctx.Err() == nil {
				t.Error(serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return &queryInfoFixture{storage: storage, server: server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

// From fs_info_wire_test.go.
func checkFilesystemSpace(t *testing.T, data []byte, class wire.FilesystemInfoClass, space smb.Space) {
	t.Helper()
	if got := binary.LittleEndian.Uint64(data[0:8]); got != space.Capacity/4096 {
		t.Fatalf("class %d total units = %d, want %d", class, got, space.Capacity/4096)
	}
	if got := binary.LittleEndian.Uint64(data[8:16]); got != space.Available/4096 {
		t.Fatalf("class %d caller units = %d, want %d", class, got, space.Available/4096)
	}
	sectorOffset := 16
	if class == wire.ClassFilesystemFullSize {
		if got := binary.LittleEndian.Uint64(data[16:24]); got != space.Free/4096 {
			t.Fatalf("actual available at offset 16 = %d, want %d", got, space.Free/4096)
		}
		sectorOffset = 24
	}
	if len(data) != sectorOffset+8 || binary.LittleEndian.Uint32(data[sectorOffset:sectorOffset+4]) != 8 || binary.LittleEndian.Uint32(data[sectorOffset+4:sectorOffset+8]) != 512 {
		t.Fatalf("class %d allocation geometry = %x", class, data)
	}
}

// From fuzz_storage_test.go.
const fuzzResetPageSize = 64

// From fuzz_storage_test.go.
func resetFuzzStorage(ctx context.Context, storage smb.Storage, root smb.Attr) error {
	if err := removeFuzzChildren(ctx, storage, root.Inode); err != nil {
		return err
	}
	return storage.SetAttr(ctx, smb.ObjectKey{Inode: root.Inode}, smb.AttrChange{
		Created: &root.Created, Accessed: &root.Accessed, Modified: &root.Modified,
		Changed: &root.Changed, Attributes: &root.Attributes,
	})
}

// From fuzz_storage_test.go.
func removeFuzzChildren(ctx context.Context, storage smb.Storage, parent smb.Inode) error {
	var cookie smb.Cookie
	for {
		entries, err := storage.ReadDir(ctx, parent, cookie, fuzzResetPageSize)
		if err != nil {
			return fmt.Errorf("list fuzz directory: %w", err)
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			if entry.Attr.Kind == smb.KindDirectory {
				if err := removeFuzzChildren(ctx, storage, entry.Attr.Inode); err != nil {
					return err
				}
			}
			if err := storage.Remove(ctx, smb.Name{Parent: parent, Base: entry.Name}, entry.Attr.Inode); err != nil {
				return fmt.Errorf("remove fuzz entry %q: %w", entry.Name, err)
			}
			cookie = entry.Next
		}
	}
}

// From fuzz_storage_test.go.
func fuzzRuntimeWorkers(t *testing.T) int {
	t.Helper()
	var stacks bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&stacks, 2); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, stack := range strings.Split(stacks.String(), "\n\n") {
		if strings.Contains(stack, "internal/juicefs/pkg/chunk.NewCachedStore.func") ||
			strings.Contains(stack, ").cleanupCache(") ||
			strings.Contains(stack, ").checkReadBuffer(") ||
			strings.Contains(stack, ").flushAll(") {
			count++
		}
	}
	return count
}

// From io_test.go.
type ioStore struct {
	object.ObjectStorage
	put  func(context.Context, string, io.Reader) error
	puts atomic.Int64
	fail atomic.Bool
}

// From io_test.go.
func (store *ioStore) Put(ctx context.Context, key string, src io.Reader, getters ...object.AttrGetter) error {
	store.puts.Add(1)
	if store.fail.Load() {
		return smb.ErrIO
	}
	if store.put != nil {
		return store.put(ctx, key, src)
	}
	return store.ObjectStorage.Put(ctx, key, src, getters...)
}

// From io_test.go.
type ioFixture struct {
	adapter *smbfs.FS
	native  *jfs.FileSystem
	store   *ioStore
	config  *vfs.Config
}

// From io_test.go.
func newIOFixture(t *testing.T, barrier smbfs.MetadataBarrier) *ioFixture {
	t.Helper()
	return newConfiguredIOFixture(t, barrier, nil, 0)
}

// From io_test.go.
func newConfiguredIOFixture(t *testing.T, barrier smbfs.MetadataBarrier, configure func(string, *meta.Config, *chunk.Config, *ioStore), readWindow time.Duration) *ioFixture {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	store := &ioStore{ObjectStorage: blob}
	mc := meta.DefaultConf()
	mc.NoBGJob, mc.MaxDeletes, mc.Retries = true, 0, 0
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: time.Second, PutTimeout: time.Second}
	if configure != nil {
		configure(dir, mc, &cc, store)
	}
	database := filepath.Join(dir, "meta.db")
	metadata, err := meta.NewSQLite(database, mc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := metadata.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	format := meta.Format{Name: "server-io", UUID: "server-io", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true}
	if initErr := metadata.Init(&format, true); initErr != nil {
		t.Fatal(initErr)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if eno := metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	if sessionErr := metadata.NewSession(true); sessionErr != nil {
		t.Fatal(sessionErr)
	}
	chunks := chunk.NewCachedStore(store, cc, nil)
	config := &vfs.Config{Meta: mc, Format: format, Chunk: &cc}
	native, err := jfs.NewFileSystem(config, metadata, chunks, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := native.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if barrier == nil {
		barrier, err = smbfs.NewMetadataBarrier(database)
		if err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := smbfs.New(smbfs.Options{Filesystem: native, Barrier: barrier, MetadataPath: database, Config: config, Store: chunks, ReadRetryWindow: readWindow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adapter.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return &ioFixture{adapter: adapter, native: native, store: store, config: config}
}

// From io_test.go.
func insertIOOpen(t *testing.T, server *Server, session smbtest.Session, path string, granted uint32) state.Open {
	t.Helper()
	storage := server.options.Storage
	selected, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !selected.Exists {
		selected, err = storage.Create(t.Context(), selected.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	token, status := server.options.State.Reserve(state.OpenRequest{Object: selected.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: server.options.Account.User, Share: server.options.ShareName, GrantedAccess: granted, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := storage.Open(t.Context(), selected.Object, createStorageAccess(granted, false))
	if err != nil {
		if abortStatus := server.options.State.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(err)
	}
	open, status := server.options.State.Commit(token, state.Grant{Handle: handle})
	if status != smb.StatusSuccess {
		if err := storage.Close(t.Context(), handle); err != nil {
			t.Error(err)
		}
		if abortStatus := server.options.State.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(status)
	}
	return open
}

// From io_test.go.
func ioMessage(session smbtest.Session, id uint64, command wire.Command, body []byte, charge uint16) wire.Message {
	return wire.Message{Header: wire.Header{Command: command, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: charge, Credit: 32}, Body: body}
}

// From io_test.go.
func ioRoundTrip(ctx context.Context, t *testing.T, client *smbtest.Client, message wire.Message) wire.Message {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatal("expected one reply")
	}
	result := response.Messages[0]
	if result.Header.Status == smb.StatusPending {
		pending := result.Header
		response, err = client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatal("expected one final reply")
		}
		result = response.Messages[0]
		if result.Header.AsyncID != pending.AsyncID || result.Header.Flags&wire.FlagAsync == 0 || result.Header.Credit != 0 {
			t.Fatal("invalid async completion")
		}
	}
	if result.Header.MessageID != message.Header.MessageID || result.Header.Command != message.Header.Command {
		t.Fatalf("wrong reply: %+v", result.Header)
	}
	return result
}

// From late_async_order_test.go.
func testLateAsyncOrder(t *testing.T, simultaneous bool) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	statuses := []smb.Status{smb.StatusFileLockConflict, smb.StatusAccessDenied, smb.StatusDiskFull}
	gates := []*asyncGate{newAsyncGate(), newAsyncGate(), newAsyncGate()}
	if simultaneous {
		gates[1], gates[2] = gates[0], gates[0]
	}
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, message wire.Message) (reply, error) {
		if message.Header.MessageID < 1 || message.Header.MessageID > 3 {
			return reply{}, errors.New("unexpected controlled request ID")
		}
		index := int(message.Header.MessageID) - 1
		select {
		case <-gates[index].done:
			return reply{status: statuses[index]}, nil
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	client, ctx := corePipeClient(t, server)
	for _, gate := range gates {
		t.Cleanup(gate.release)
	}
	exchange(ctx, t, client, negotiateMessage(t, 3))
	pending := make(map[uint64]wire.Header)
	asyncIDs := make(map[uint64]bool)
	for id := uint64(1); id <= 3; id++ {
		header := exchange(ctx, t, client, asyncMessage(t, wire.Read, id))[0].Header
		if header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || asyncIDs[header.AsyncID] {
			t.Fatalf("pending request lost its own async identity: %+v", header)
		}
		pending[id] = header
		asyncIDs[header.AsyncID] = true
	}
	if simultaneous {
		gates[0].release()
	}
	seen := make(map[uint64]bool)
	for wantID := uint64(3); wantID > 0; wantID-- {
		if !simultaneous {
			gates[wantID-1].release()
		}
		final, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(final.Messages) != 1 {
			t.Fatalf("final frame contains %d replies", len(final.Messages))
		}
		header := final.Messages[0].Header
		interim, exists := pending[header.MessageID]
		if !exists || seen[header.MessageID] || !simultaneous && header.MessageID != wantID {
			t.Fatalf("unexpected completion: %+v", header)
		}
		assertLateAsyncIdentity(t, header, interim, statuses[header.MessageID-1])
		seen[header.MessageID] = true
	}
	// This also detects an extra final reply left ahead of unrelated traffic.
	response := exchange(ctx, t, client, echo(t, 4))
	if len(response) != 1 || response[0].Header.MessageID != 4 || response[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("completion leaked into the next exchange: %+v", response)
	}
}

// From late_async_order_test.go.
func assertLateAsyncIdentity(t *testing.T, final, pending wire.Header, status smb.Status) {
	t.Helper()
	if final.MessageID != pending.MessageID || final.AsyncID != pending.AsyncID || final.SessionID != pending.SessionID || final.Command != pending.Command || final.Flags&wire.FlagAsync == 0 || final.Status != status || final.Credit != 0 || final.CreditCharge != pending.CreditCharge || final.TreeID != 0 {
		t.Fatalf("final reply lost identity, status or credits: final %+v, pending %+v, want status %v", final, pending, status)
	}
}

// From late_async_sender_test.go.
// The async producer reports send failures through the server logger.
type asyncReplyErrors chan error

// From late_async_sender_test.go.
func (asyncReplyErrors) Enabled(context.Context, slog.Level) bool { return true }

// From late_async_sender_test.go.
func (logs asyncReplyErrors) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "async reply failed" {
		return nil
	}
	replyErr := errors.New("async failure log lacks an error")
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" {
			if err, ok := attr.Value.Any().(error); ok {
				replyErr = err
			}
		}
		return true
	})
	logs <- replyErr
	return nil
}

// From late_async_sender_test.go.
func (logs asyncReplyErrors) WithAttrs([]slog.Attr) slog.Handler { return logs }

// From late_async_sender_test.go.
func (logs asyncReplyErrors) WithGroup(string) slog.Handler { return logs }

// From late_async_sender_test.go.
func controlledAsyncExchange(ctx context.Context, t *testing.T, client *smbtest.Client, conn *controlledConn, message wire.Message) wire.Message {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	call := waitWrite(ctx, t, conn)
	call.release <- nil
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("controlled exchange returned %d replies", len(response.Messages))
	}
	return response.Messages[0]
}

// From late_async_sender_test.go.
// Hold the failing write until the async producer is waiting on its own queued
// send. Queue observation is only a scheduling barrier; assertions use the
// producer's error, transport bytes and ServeConn's return.
func waitAsyncSendQueued(ctx context.Context, t *testing.T, server *Server) {
	t.Helper()
	server.mu.Lock()
	var active *connection
	for conn := range server.connections {
		active = conn
	}
	server.mu.Unlock()
	if active == nil {
		t.Fatal("no active connection")
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		active.sender.mu.Lock()
		queued := len(active.sender.queue) > 0
		active.sender.mu.Unlock()
		if queued {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// From late_async_test.go.
type asyncGate struct {
	done chan struct{}
	once sync.Once
}

// From late_async_test.go.
func newAsyncGate() *asyncGate { return &asyncGate{done: make(chan struct{})} }

// From late_async_test.go.
func (gate *asyncGate) release() { gate.once.Do(func() { close(gate.done) }) }

// From late_async_test.go.
// Observe transport writes, including attempts made after the server closes it.
type observedAsyncConn struct {
	net.Conn
	closeErr   error
	closed     chan struct{}
	closeOnce  sync.Once
	writes     atomic.Int32
	lateWrites atomic.Int32
}

// From late_async_test.go.
func (conn *observedAsyncConn) Write(data []byte) (int, error) {
	conn.writes.Add(1)
	select {
	case <-conn.closed:
		conn.lateWrites.Add(1)
	default:
	}
	return conn.Conn.Write(data)
}

// From late_async_test.go.
func (conn *observedAsyncConn) Close() error {
	conn.closeOnce.Do(func() {
		conn.closeErr = conn.Conn.Close()
		close(conn.closed)
	})
	return conn.closeErr
}

// From late_async_test.go.
type lateAsyncPeer struct {
	client *smbtest.Client
	ctx    context.Context
	done   chan struct{}
	err    error // Published before done closes.
}

// From late_async_test.go.
func serveLateAsyncPipe(t *testing.T, server *Server, local, remote net.Conn) *lateAsyncPeer {
	t.Helper()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(errors.Join(err, local.Close(), remote.Close()))
	}
	ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
	// Keep the connection alive independently of test cleanup cancellation.
	serverCtx := context.WithoutCancel(t.Context())
	connCtx, cancel := context.WithCancel(serverCtx)
	connection, err := server.addConnection(connCtx, cancel, local)
	if err != nil {
		cancel()
		stop()
		t.Fatal(errors.Join(err, client.Close(), local.Close()))
	}
	// These controlled handlers isolate async plumbing, not authentication.
	connection.sessions[77] = &sessionEntry{identity: Session{SessionID: 77}, active: true, trees: map[uint32]Tree{12: {TreeID: 12, Share: "backup"}}}
	peer := &lateAsyncPeer{client: client, ctx: ctx, done: make(chan struct{})}
	go func() {
		peer.err = server.runConnection(connCtx, connection)
		close(peer.done)
	}()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-peer.done:
			if peer.err != nil {
				t.Logf("ServeConn: %v", peer.err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ServeConn did not drain late work")
		}
		stop()
		shutdownCtx, shutdownCancel := context.WithTimeout(serverCtx, 3*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Error(err)
		}
	})
	return peer
}

// From late_async_test.go.
func waitAsyncSignal(ctx context.Context, t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// From late_async_test.go.
func waitAsyncResult(ctx context.Context, t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

// From late_async_test.go.
func testLateAsyncShutdown(t *testing.T, deadline bool) {
	options := testOptions(t)
	logs := make(asyncReplyErrors, 4)
	options.Logger = slog.New(logs)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := newAsyncGate()
	canceled := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		close(canceled)
		<-release.done
		return reply{status: smb.StatusFileLockConflict}, nil
	}
	local, remote := net.Pipe()
	conn := &observedAsyncConn{Conn: local, closed: make(chan struct{})}
	peer := serveLateAsyncPipe(t, server, conn, remote)
	t.Cleanup(release.release)
	exchange(peer.ctx, t, peer.client, negotiateMessage(t, 1))
	pending := exchange(peer.ctx, t, peer.client, asyncMessage(t, wire.Read, 1))[0]
	if pending.Header.Status != smb.StatusPending {
		t.Fatalf("request did not become pending: %+v", pending.Header)
	}
	shutdownCtx := context.WithoutCancel(peer.ctx)
	if deadline {
		var stop context.CancelFunc
		shutdownCtx, stop = context.WithTimeout(peer.ctx, 50*time.Millisecond)
		defer stop()
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- server.Shutdown(shutdownCtx) }()
	waitAsyncSignal(peer.ctx, t, canceled)
	waitAsyncSignal(peer.ctx, t, conn.closed)
	if _, err := peer.client.Receive(peer.ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("shutdown leaked a reply: %v", err)
	}
	writes := conn.writes.Load()
	if deadline {
		if err := waitAsyncResult(peer.ctx, t, shutdown); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown ignored its caller's bound: %v", err)
		}
	} else {
		select {
		case err := <-shutdown:
			t.Fatalf("shutdown returned before late work finished: %v", err)
		default:
		}
	}
	release.release()
	waitAsyncSignal(peer.ctx, t, peer.done)
	if !errors.Is(peer.err, context.Canceled) {
		t.Fatalf("shutdown did not cancel the connection: %v", peer.err)
	}
	if deadline {
		if err := server.Shutdown(peer.ctx); err != nil {
			t.Fatalf("shutdown did not finish after its caller expired: %v", err)
		}
	} else if err := waitAsyncResult(peer.ctx, t, shutdown); err != nil {
		t.Fatal(err)
	}
	if conn.writes.Load() != writes || conn.lateWrites.Load() != 0 {
		t.Fatal("late work wrote a reply after shutdown closed the transport")
	}
	assertNoAsyncFailure(t, logs)
}

// From late_async_test.go.
func assertNoAsyncFailure(t *testing.T, logs asyncReplyErrors) {
	t.Helper()
	select {
	case err := <-logs:
		t.Fatalf("late result reached the stopped sender: %v", err)
	default:
	}
}

// From late_session_test.go.
func checkLateSessionCleanup(t *testing.T, command wire.Command, cipher, signing uint16, wantStatus smb.Status) {
	t.Helper()
	options := testOptions(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := newAsyncGate()
	canceled := newAsyncGate()
	closedWhileActive := make(chan int32, 1)
	body, err := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("L")})
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		canceled.release()
		<-release.done
		closedWhileActive <- storage.closed.Load()
		if wantStatus == smb.StatusCancelled {
			return reply{}, ctx.Err()
		}
		if wantStatus == smb.StatusIODeviceError {
			return reply{}, smb.ErrIO
		}
		return reply{body: body}, nil
	}
	client, ctx, session := loginClient(t, server, cipher, signing)
	owner := onlyConnection(t, server)
	// Release the handler before the pipe fixture tries to drain it on failure.
	t.Cleanup(release.release)
	open := insertSessionOpen(t, server, session, false, 50)
	request := treeRequest(t, session, session.NextMessageID, wire.Read)
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	pending := receiveLateSessionReply(ctx, t, client, cipher)
	if pending.Header.Status != smb.StatusPending || pending.Header.Flags&wire.FlagAsync == 0 || pending.Header.AsyncID == 0 || pending.Header.MessageID != request.Header.MessageID || pending.Header.SessionID != session.SessionID || pending.Header.Credit == 0 {
		t.Fatalf("pending: %+v", pending.Header)
	}
	cleanup := treeRequest(t, session, session.NextMessageID+1, command)
	if err := client.Send(ctx, []wire.Message{cleanup}); err != nil {
		t.Fatal(err)
	}
	waitAsyncSignal(ctx, t, canceled.done)
	if storage.closed.Load() != 0 {
		t.Fatal("cleanup closed storage before the canceled handler returned")
	}
	owner.sessionMu.RLock()
	entry := owner.sessions[session.SessionID]
	removed := entry == nil
	if command == wire.TreeDisconnect && entry != nil {
		_, exists := entry.trees[session.TreeID]
		removed = !exists
	}
	retained := owner.replyProtection[request.Header.MessageID].session != nil
	owner.sessionMu.RUnlock()
	if !removed || !retained {
		t.Fatal("cleanup did not retire the identity while retaining the pending reply keys")
	}
	release.release()
	checkLateSessionReplies(ctx, t, client, cipher, pending.Header, cleanup.Header, wantStatus)
	if storage.closed.Load() != 1 {
		t.Fatalf("cleanup closed %d handles, want 1", storage.closed.Load())
	}
	if closed := <-closedWhileActive; closed != 0 {
		t.Fatalf("handler used storage after cleanup closed %d handles", closed)
	}
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("cleanup left the retired identity's open")
	}
	drained := make(chan struct{})
	go func() {
		owner.workers.Wait()
		close(drained)
	}()
	waitAsyncSignal(ctx, t, drained)
	owner.sessionMu.RLock()
	keys, holders := len(owner.replyProtection), len(owner.inflight)
	owner.sessionMu.RUnlock()
	owner.pendingMu.Lock()
	pendingCount := len(owner.pending)
	owner.pendingMu.Unlock()
	if keys != 0 || holders != 0 || pendingCount != 0 {
		t.Fatalf("terminal reply retained work or keys: keys %d, holders %d, pending %d", keys, holders, pendingCount)
	}
	checkRetiredSessionTraffic(ctx, t, client, session, command, cipher)
}

// From late_session_test.go.
// Cleanup drains the handler, not the sender. Either reply can arrive first.
func checkLateSessionReplies(ctx context.Context, t *testing.T, client *smbtest.Client, cipher uint16, pending, cleanup wire.Header, wantStatus smb.Status) {
	t.Helper()
	seen := make(map[uint64]bool)
	for range 2 {
		response := receiveLateSessionReply(ctx, t, client, cipher)
		header := response.Header
		if seen[header.MessageID] {
			t.Fatalf("duplicate reply: %+v", header)
		}
		seen[header.MessageID] = true
		switch header.MessageID {
		case pending.MessageID:
			if header.Command != wire.Read || header.Status != wantStatus || header.SessionID != pending.SessionID || header.AsyncID != pending.AsyncID || header.Flags&wire.FlagAsync == 0 || header.TreeID != 0 || header.Credit != 0 || header.CreditCharge != pending.CreditCharge {
				t.Fatalf("terminal reply: %+v", header)
			}
			if wantStatus == smb.StatusSuccess {
				read, err := wire.DecodeReadResponse(response)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(read.Data, []byte("L")) {
					t.Fatalf("late result changed: %q", read.Data)
				}
			}
		case cleanup.MessageID:
			if header.Command != cleanup.Command || header.Status != smb.StatusSuccess || header.SessionID != cleanup.SessionID || header.TreeID != cleanup.TreeID || header.Flags&wire.FlagAsync != 0 {
				t.Fatalf("cleanup reply: %+v", header)
			}
		default:
			t.Fatalf("unexpected reply: %+v", header)
		}
	}
}

// From late_session_test.go.
func receiveLateSessionReply(ctx context.Context, t *testing.T, client *smbtest.Client, cipher uint16) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply, got %d", len(response.Messages))
	}
	message := response.Messages[0]
	encrypted := bytes.HasPrefix(response.Raw, []byte{0xfd, 'S', 'M', 'B'})
	if encrypted != (cipher != 0) {
		t.Fatal("reply lost its negotiated protection")
	}
	if encrypted && (message.Header.Flags&wire.FlagSigned != 0 || message.Header.Signature != [16]byte{}) {
		t.Fatal("encrypted reply was separately signed")
	}
	if !encrypted && message.Header.Status != smb.StatusPending && message.Header.Flags&wire.FlagSigned == 0 {
		t.Fatal("terminal plaintext reply was not signed")
	}
	return message
}

// From late_session_test.go.
func checkRetiredSessionTraffic(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, command wire.Command, cipher uint16) {
	t.Helper()
	stale := treeRequest(t, session, session.NextMessageID+2, wire.Read)
	if command == wire.TreeDisconnect {
		if err := client.Send(ctx, []wire.Message{stale}); err != nil {
			t.Fatal(err)
		}
		response := receiveLateSessionReply(ctx, t, client, cipher)
		if response.Header.MessageID != stale.Header.MessageID || response.Header.Status != smb.StatusNetworkNameDeleted {
			t.Fatalf("retired tree reply or duplicate completion: %+v", response.Header)
		}
		response = exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+3))[0]
		if response.Header.Status != smb.StatusSuccess || response.Header.MessageID != session.NextMessageID+3 {
			t.Fatalf("surviving session ECHO: %+v", response.Header)
		}
		return
	}
	// A removed session cannot decrypt new traffic. Raw plaintext probes must
	// receive unprotected refusals, not replies made with its retired keys.
	for _, request := range []wire.Message{stale, echo(t, session.NextMessageID+3)} {
		payload, err := wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		sendPayload(ctx, t, client, payload)
		payload, err = client.ReceiveRaw(ctx)
		if err != nil {
			t.Fatal(err)
		}
		messages, err := wire.Split(payload)
		if err != nil {
			t.Fatalf("removed session keys protected new traffic: %v", err)
		}
		if len(messages) != 1 {
			t.Fatalf("unexpected replies after cleanup: %d", len(messages))
		}
		header := messages[0].Header
		want := smb.StatusUserSessionDeleted
		if request.Header.Command == wire.Echo {
			want = smb.StatusSuccess
		}
		if header.MessageID != request.Header.MessageID || header.Command != request.Header.Command || header.SessionID != request.Header.SessionID || header.Status != want || header.Flags&(wire.FlagSigned|wire.FlagAsync) != 0 || header.Signature != [16]byte{} {
			t.Fatalf("retired identity reply or duplicate completion: %+v", header)
		}
	}
}

// From lease_ack_guard_test.go.
func checkLeaseAckIdentityGuard(t *testing.T, mode string) {
	t.Helper()
	server, holder, _ := newCreateLeaseClients(t)
	other := loginCreateLeaseClient(t, server, 2)
	opened := holder.create(t, leaseCreateRequest("guarded-ack"), leaseV2(4, 7))
	open, status := server.options.State.Find(state.FileID(opened.Reply.ID), state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	done := startServerBreak(holder.ctx, server, open, smb.LeaseRead|smb.LeaseHandle)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := server.sessionConnection(other.session.SessionID)
	if owner == nil {
		t.Fatal("second login has no canonical owner")
	}
	ctx, cancel := context.WithCancel(other.ctx)
	defer cancel()
	header := wire.Header{Command: wire.OplockBreak, SessionID: other.session.SessionID, MessageID: other.next}
	request, operation, status := owner.resolveRequest(header, cancel)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	defer owner.finishRequest(operation)
	want := smb.StatusUserSessionDeleted
	switch mode {
	case "removed session":
		owner.removeSession(other.session.SessionID, "")
	case "inactive session":
		owner.sessionMu.Lock()
		owner.sessions[other.session.SessionID].active = false
		owner.sessionMu.Unlock()
	case "removed tree":
		owner.sessionMu.Lock()
		request.Tree = owner.sessions[other.session.SessionID].trees[other.session.TreeID]
		delete(owner.sessions[other.session.SessionID].trees, other.session.TreeID)
		owner.sessionMu.Unlock()
		want = smb.StatusNetworkNameDeleted
	case "canceled":
		cancel()
	}
	body, err := wire.EncodeLeaseBreakRequest(wire.LeaseBreakRequest{Key: notification.Key, State: notification.NewState})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handleOplockBreak(ctx, request, wire.Message{Header: header, Body: body})
	if mode == "canceled" {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled ACK = %+v, %v", result, err)
		}
	} else if err != nil || result.status != want {
		t.Fatalf("retired ACK = %#x, %v; want %#x", result.status, err, want)
	}
	current, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || !current.Breaking || current.State != 7 || current.BreakTo != 3 || current.Epoch != notification.Epoch {
		t.Fatalf("refused ACK changed captured break: %+v", current)
	}
	holder.ack(t, notification)
	finishServerBreak(holder.ctx, t, done)
}

// From lease_break_cleanup_test.go.
func joinDurableLease(t *testing.T, server *Server, session smbtest.Session, open state.Open) state.Open {
	t.Helper()
	request := state.OpenRequest{Object: open.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: open.User, Share: open.Share, ClientGUID: open.ClientGUID, CreateGUID: state.GUID{99}, Sharing: 7}
	grant := state.Grant{Handle: cleanupHandle{object: open.Object}, DurableTimeout: smb.DefaultDurableTimeout, Lease: state.Lease{ClientGUID: open.ClientGUID, Key: open.LeaseKey, State: smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite}}
	return commitLeaseOpen(t, server, request, grant)
}

// From lease_break_lifecycle_test.go.
// Done observes entry into the send or break wait, without advancing the clock.
type observedBreakWait struct {
	context.Context
	waiting chan struct{}
	calls   atomic.Int32
	after   int32
}

// From lease_break_lifecycle_test.go.
func (ctx *observedBreakWait) Done() <-chan struct{} {
	if ctx.calls.Add(1) == ctx.after {
		close(ctx.waiting)
	}
	return ctx.Context.Done()
}

// From lease_break_lifecycle_test.go.
func fixedClockLeaseServer(t *testing.T) (*Server, *cleanupStorage) {
	t.Helper()
	options := testOptions(t)
	now := time.Now()
	options.Now = func() time.Time { return now }
	var err error
	options.State, err = state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server, storage
}

// From lease_break_lifecycle_test.go.
type observedBreakConn struct {
	net.Conn
	started chan struct{}
	armed   atomic.Bool
	once    sync.Once
}

// From lease_break_lifecycle_test.go.
func (conn *observedBreakConn) Write(data []byte) (int, error) {
	if conn.armed.Load() {
		conn.once.Do(func() { close(conn.started) })
	}
	return conn.Conn.Write(data)
}

// From lease_break_lifecycle_test.go.
func checkBlockedLeaseBreakReplacement(t *testing.T, cleanupError bool) {
	t.Helper()
	server, storage := fixedClockLeaseServer(t)
	if cleanupError {
		storage.closeErr = smb.ErrIO
	}
	local, remote := net.Pipe()
	conn := &observedBreakConn{Conn: local, started: make(chan struct{})}
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(errors.Join(err, local.Close(), remote.Close()))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, conn) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case serveErr := <-served:
			if serveErr != nil {
				t.Logf("old transport: %v", serveErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("old transport did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
	conn.armed.Store(true)
	done := startServerBreak(ctx, server, open, smb.LeaseRead|smb.LeaseHandle)
	waitAsyncSignal(ctx, t, conn.started)
	freshClient, freshCtx := pipeClient(t, server)
	fresh, err := freshClient.Login(freshCtx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}, PreviousSessionID: session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	breakErr := waitAsyncResult(ctx, t, done)
	if cleanupError {
		if !errors.Is(breakErr, smb.ErrIO) {
			t.Fatalf("detached cleanup error: %v", breakErr)
		}
	} else if breakErr != nil {
		t.Fatal(breakErr)
	}
	if storage.closed.Load() != 1 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("blocked send kept detached rights or handle")
	}
	// Completion does not cancel the ordered write or discard its captured keys.
	notification, err := client.WaitLeaseBreak(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: [16]byte(open.LeaseKey), Epoch: 8, Flags: 1, CurrentState: smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite, NewState: smb.LeaseRead | smb.LeaseHandle}
	if notification != want {
		t.Fatalf("captured notification: %+v, want %+v", notification, want)
	}
	assertRawStatus(ctx, t, client, echo(t, session.NextMessageID), smb.StatusSuccess)
	if response := exchange(freshCtx, t, freshClient, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("replacement session is not usable")
	}
}

// From lease_break_protection_test.go.
func checkPlaintextBreak(t *testing.T, payload []byte) wire.LeaseBreakNotification {
	t.Helper()
	messages, err := wire.Split(payload)
	if err != nil || len(messages) != 1 {
		t.Fatalf("notification frame: %+v, %v", messages, err)
	}
	header := messages[0].Header
	if header.Command != wire.OplockBreak || header.MessageID != ^uint64(0) || header.SessionID != 0 || header.TreeID != 0 || header.Credit != 0 || header.CreditCharge != 0 || header.Flags != wire.FlagResponse || header.Signature != [16]byte{} {
		t.Fatalf("unsolicited header: %+v", header)
	}
	notification, err := wire.DecodeLeaseBreakNotification(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	return notification
}

// From lease_break_test.go.
func leaseServer(t *testing.T, cipher, signing uint16) (*Server, *smbtest.Client, context.Context, smbtest.Session, *cleanupStorage) {
	t.Helper()
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, cipher, signing)
	return server, client, ctx, session, storage
}

// From lease_break_test.go.
func insertLeaseOpen(t *testing.T, server *Server, session smbtest.Session, inode smb.Inode, rights uint32, durable bool) state.Open {
	t.Helper()
	request := state.OpenRequest{Object: smb.ObjectKey{Inode: inode}, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: state.GUID{2}, Sharing: 7}
	grant := state.Grant{Handle: cleanupHandle{object: request.Object}, Lease: state.Lease{ClientGUID: request.ClientGUID, Key: state.GUID{byte(inode & 0xff)}, State: rights, Epoch: 7}}
	if durable {
		request.CreateGUID = grant.Lease.Key
		grant.DurableTimeout = time.Minute
	}
	return commitLeaseOpen(t, server, request, grant)
}

// From lease_break_test.go.
func commitLeaseOpen(t *testing.T, server *Server, request state.OpenRequest, grant state.Grant) state.Open {
	t.Helper()
	reservation, status := server.options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, grant)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

// From lease_break_test.go.
func startServerBreak(ctx context.Context, server *Server, open state.Open, target uint32) <-chan error {
	done := make(chan error, 1)
	go func() { done <- server.BreakLeases(ctx, open.Object, state.GUID{9}, state.GUID{9}, target) }()
	return done
}

// From lease_break_test.go.
func finishServerBreak(ctx context.Context, t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// From lease_break_test.go.
func acknowledgeBreak(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, key state.GUID, rights uint32) wire.Message {
	t.Helper()
	header := wire.Header{SessionID: session.SessionID, MessageID: id, CreditCharge: 1, Credit: 1}
	if err := client.SendLeaseBreakAcknowledgment(ctx, header, wire.LeaseBreakRequest{Key: [16]byte(key), State: rights}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("ack reply count: %d", len(response.Messages))
	}
	return response.Messages[0]
}

// From lease_break_test.go.
func checkLeaseBreakNotification(t *testing.T, cipher, signing uint16, current uint32) {
	t.Helper()
	server, client, ctx, session, _ := leaseServer(t, cipher, signing)
	open := insertLeaseOpen(t, server, session, 2, current, false)
	target, flags := uint32(0), uint32(0)
	if current != smb.LeaseRead {
		target, flags = smb.LeaseRead|smb.LeaseHandle, 1
	}
	done := startServerBreak(ctx, server, open, target)
	notification, err := client.WaitLeaseBreak(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: [16]byte(open.LeaseKey), CurrentState: current, NewState: target, Epoch: 8, Flags: flags}
	if notification != want {
		t.Fatalf("notification: %+v, want %+v", notification, want)
	}
	if flags != 0 {
		select {
		case err := <-done:
			t.Fatalf("wait ended before ack: %v", err)
		default:
		}
		response := acknowledgeBreak(ctx, t, client, session, session.NextMessageID, open.LeaseKey, target)
		ack, decodeErr := wire.DecodeLeaseBreakResponse(response)
		if decodeErr != nil || ack.Key != notification.Key || ack.State != target || ack.Duration != 0 || ack.Flags != 0 {
			t.Fatalf("ack response: %+v, %v", response, decodeErr)
		}
	}
	finishServerBreak(ctx, t, done)
}

// From lease_break_test.go.
func checkLeaseBreakWait(t *testing.T, finish string) {
	t.Helper()
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	var nanos atomic.Int64
	nanos.Store(time.Now().UnixNano())
	options.Now = func() time.Time { return time.Unix(0, nanos.Load()) }
	var err error
	options.State, err = state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	open := insertLeaseOpen(t, server, session, 2, smb.LeaseRead|smb.LeaseHandle, false)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed := options.State.BreakChanges()
	done := startServerBreak(waitCtx, server, open, smb.LeaseRead)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if finish == "timeout" || finish == "cancel" {
		if _, receiveErr := client.WaitLeaseBreak(ctx); receiveErr != nil {
			t.Fatal(receiveErr)
		}
	}
	if finish == "cancel" || finish == "cancel without reading" {
		cancel()
		select {
		case waitErr := <-done:
			if !errors.Is(waitErr, context.Canceled) || !options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
				t.Fatalf("cancel lost pending break: %v", waitErr)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return
	}
	nanos.Add(int64(state.LeaseBreakTimeout))
	if actions := options.State.ExpireBreaks(); len(actions) != 0 {
		t.Fatal("expiry closed an attached open")
	}
	finishServerBreak(ctx, t, done)
}

// From lease_continuation_test.go.
func checkQueuedLeaseBreakCancellation(t *testing.T, cancelQueued bool) {
	t.Helper()
	server, holder, _ := newCreateLeaseClients(t)
	created := holder.create(t, leaseCreateRequest("cancel-queued-break"), leaseV2(1, 7))
	open, status := server.options.State.Find(state.FileID(created.Reply.ID), state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("created lease open: %#x", status)
	}
	originalCtx, cancelOriginal := context.WithCancel(holder.ctx)
	defer cancelOriginal()
	originalDone := startServerBreak(originalCtx, server, open, 3)
	first, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	captured, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists {
		t.Fatal("captured lease missing")
	}
	changed := server.options.State.BreakChanges()
	queuedCtx, cancelStronger := context.WithCancel(holder.ctx)
	defer cancelStronger()
	queuedDone := startServerBreak(queuedCtx, server, open, 0)
	waitAsyncSignal(holder.ctx, t, changed)
	canceled, survivor, cancel := originalDone, queuedDone, cancelOriginal
	if cancelQueued {
		canceled, survivor, cancel = queuedDone, originalDone, cancelStronger
	}
	cancel()
	if waitErr := waitAsyncResult(holder.ctx, t, canceled); !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("canceled waiter: %v", waitErr)
	}
	current, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || current.State != captured.State || current.BreakTo != captured.BreakTo || current.Epoch != captured.Epoch || current.Deadline != captured.Deadline || current.EffectiveState() != 0 {
		t.Fatalf("cancellation changed captured stage or discarded queued revocation: %+v", current)
	}
	holder.ack(t, first)
	second, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil || second.CurrentState != 3 || second.NewState != 1 || second.Epoch != first.Epoch || second.Flags != 1 {
		t.Fatalf("continuation after cancellation: %+v, error %v", second, err)
	}
	select {
	case breakErr := <-survivor:
		t.Fatalf("surviving waiter completed before the next ACK: %v", breakErr)
	default:
	}
	holder.ack(t, second)
	last, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil || last.CurrentState != 1 || last.NewState != 0 || last.Epoch != first.Epoch || last.Flags != 0 {
		t.Fatalf("final continuation after cancellation: %+v, error %v", last, err)
	}
	finishServerBreak(holder.ctx, t, survivor)
	holder.close(t, wire.FileID(open.ID))
}

// From lease_continuation_test.go.
func checkQueuedLeaseBreakCreates(t *testing.T, cipher uint16) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	holder := loginCreateLeaseClientWithCipher(t, server, 2, cipher)
	reader := loginCreateLeaseClientWithCipher(t, server, 3, cipher)
	writer := loginCreateLeaseClientWithCipher(t, server, 4, cipher)
	request := leaseCreateRequest("queued-break")
	opened := holder.create(t, request, leaseV2(1, 7))
	assertLeaseGrant(t, opened, 7)
	readID := reader.send(t, request, nil)
	first, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.CurrentState != 7 || first.NewState != 3 || first.Flags != 1 || first.Key != opened.Lease.Key || first.Epoch != opened.Lease.Epoch+1 {
		t.Fatalf("initial RWH-to-RH notification: %+v", first)
	}
	readDone := receiveCreateLater(reader, readID)
	assertCreateWaits(t, readDone)
	changed := server.options.State.BreakChanges()
	overwrite := request
	overwrite.DesiredAccess = fileWriteData
	overwrite.Disposition = fileOverwrite
	writeID := writer.send(t, overwrite, nil)
	writeDone := receiveCreateLater(writer, writeID)
	select {
	case <-changed:
	case <-writer.ctx.Done():
		t.Fatal(writer.ctx.Err())
	}
	assertCreateWaits(t, writeDone)
	joined := holder.create(t, request, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 7 || joined.Lease.Epoch != first.Epoch || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress {
		t.Fatalf("same-key CREATE during queued break: %+v", joined.Lease)
	}
	holder.close(t, joined.Reply.ID)
	holder.ack(t, first)
	second, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: first.Key, Epoch: first.Epoch, Flags: 1, CurrentState: 3, NewState: 1}
	if second != want {
		t.Fatalf("RH-to-R continuation: %+v, want %+v", second, want)
	}
	assertCreateWaits(t, readDone)
	assertCreateWaits(t, writeDone)
	joined = holder.create(t, request, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 3 || joined.Lease.Epoch != first.Epoch || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress {
		t.Fatalf("same-key CREATE during RH-to-R stage: %+v", joined.Lease)
	}
	holder.close(t, joined.Reply.ID)
	holder.ack(t, second)
	last, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want.Flags, want.CurrentState, want.NewState = 0, 1, 0
	if last != want {
		t.Fatalf("R-only no-ACK final notification: %+v, want %+v", last, want)
	}
	readResult := finishCreate(t, readDone)
	writeResult := finishCreate(t, writeDone)
	if readResult.Reply.Action != 1 || writeResult.Reply.Action != 3 {
		t.Fatalf("queued CREATE results: reader %+v, writer %+v", readResult.Reply, writeResult.Reply)
	}
	reader.close(t, readResult.Reply.ID)
	writer.close(t, writeResult.Reply.ID)
	holder.close(t, opened.Reply.ID)
}

// From lifecycle_test.go.
type cleanupHandle struct{ object smb.ObjectKey }

// From lifecycle_test.go.
func (handle cleanupHandle) Key() smb.ObjectKey { return handle.object }

// From lifecycle_test.go.
// This records shutdown calls, not filesystem coherence or file operations.
type cleanupStorage struct {
	smb.Storage
	closeErr  error
	removeErr error
	closed    atomic.Int32
	removed   atomic.Int32
}

// From lifecycle_test.go.
func (storage *cleanupStorage) Close(context.Context, smb.Handle) error {
	storage.closed.Add(1)
	return storage.closeErr
}

// From lifecycle_test.go.
func (*cleanupStorage) PathOf(context.Context, smb.Inode) (string, error) { return "renamed", nil }

// From lifecycle_test.go.
func (*cleanupStorage) Lookup(context.Context, string) (smb.Resolved, error) {
	return smb.Resolved{Exists: true, Object: smb.ObjectKey{Inode: 2}, Name: smb.Name{Parent: 1, Base: "renamed"}}, nil
}

// From lifecycle_test.go.
func (storage *cleanupStorage) Remove(_ context.Context, name smb.Name, inode smb.Inode) error {
	if name.Base != "renamed" || inode != 2 {
		return errors.New("shutdown used a stale deletion name")
	}
	storage.removed.Add(1)
	return storage.removeErr
}

// From lock_io_test.go.
func lockIO(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, open state.Open, write bool, data string, want smb.Status) {
	t.Helper()
	if len(data) != 8 {
		t.Fatal("lock I/O tests use eight-byte ranges")
		return
	}
	command := wire.Read
	var body []byte
	var err error
	if write {
		command = wire.Write
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{ID: wire.FileID(open.ID), Data: []byte(data)})
	} else {
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: wire.FileID(open.ID), Length: 8})
	}
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(ctx, t, client, ioMessage(session, id, command, body, 1))
	if response.Header.Status != want {
		t.Fatalf("command %d object %+v: status %#x, want %#x", command, open.Object, response.Header.Status, want)
	}
	if want != smb.StatusSuccess {
		if _, err := wire.DecodeErrorResponse(response); err != nil {
			t.Fatal(err)
		}
		return
	}
	if write {
		written, err := wire.DecodeWriteResponse(response)
		if err != nil || int(written.Count) != len(data) {
			t.Fatalf("WRITE: %+v, %v", written, err)
		}
	} else {
		read, err := wire.DecodeReadResponse(response)
		if err != nil || string(read.Data) != data {
			t.Fatalf("READ: %q, %v, want %q", read.Data, err, data)
		}
	}
}

// From lock_lifecycle_test.go.
func closeLockOpen(t *testing.T, session smbtest.Session, messageID uint64, id state.FileID) wire.Message {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: wire.FileID(id)})
	if err != nil {
		t.Fatal(err)
	}
	return ioMessage(session, messageID, wire.Close, body, 1)
}

// From lock_lifecycle_test.go.
func checkLockCompoundConflictDoesNotBlockDisconnect(t *testing.T, flags uint32) {
	t.Helper()
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	owner := insertLockOpen(t, server, session, "disconnect-lock")
	other := insertLockOpen(t, server, session, "disconnect-lock")
	id := session.NextMessageID
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
	id++
	prefix := lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 16, Length: 8, Flags: flags})
	conflict := lockMessage(t, session, id+1, state.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}, wire.LockElement{Length: 8, Flags: flags})
	conflict.Header.Flags |= wire.FlagRelated
	conflict.Header.SessionID, conflict.Header.TreeID = math.MaxUint64, math.MaxUint32
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess, smb.StatusLockNotGranted}, prefix, conflict)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	// Only this connection is running. Await its cleanup before opening another.
	drained := make(chan struct{})
	go func() {
		server.workers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("conflicting compound lock blocked connection cleanup")
	}
	for _, open := range []state.Open{owner, other} {
		if _, status := server.options.State.Find(open.ID, open.Binding); status != smb.StatusFileClosed {
			t.Fatalf("disconnected open remains: %#x", status)
		}
	}
	nextClient, nextCtx, nextSession := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	next := insertLockOpen(t, server, nextSession, "disconnect-lock")
	lockExchange(nextCtx, t, nextClient, []smb.Status{smb.StatusSuccess}, lockMessage(t, nextSession, nextSession.NextMessageID, next.ID, wire.LockElement{Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: 16, Length: 8, Flags: lockExclusive | lockFailImmediately}))
}

// From lock_test.go.
func lockServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = smbtest.NewStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// From lock_test.go.
func insertLockOpen(t *testing.T, server *Server, session smbtest.Session, path string) state.Open {
	t.Helper()
	return insertIOOpen(t, server, session, path, fileReadData|fileWriteData)
}

// From lock_test.go.
func createLockOpen(ctx context.Context, t *testing.T, server *Server, client *smbtest.Client, session smbtest.Session, id uint64, create wire.CreateRequest) state.Open {
	t.Helper()
	response := createdFile(t, fileCreate(ctx, t, client, session, id, create))
	open, status := server.options.State.Find(state.FileID(response.ID), state.Binding{SessionID: session.SessionID, TreeID: session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("CREATE open lookup: %#x", status)
	}
	return open
}

// From lock_test.go.
func lockMessage(t *testing.T, session smbtest.Session, messageID uint64, id state.FileID, elements ...wire.LockElement) wire.Message {
	t.Helper()
	body, err := wire.EncodeLockRequest(wire.LockRequest{ID: wire.FileID(id), Elements: elements})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.Lock, MessageID: messageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}, Body: body}
}

// From lock_test.go.
func lockExchange(ctx context.Context, t *testing.T, client *smbtest.Client, want []smb.Status, messages ...wire.Message) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	responses := exchange(bounded, t, client, messages...)
	if len(responses) != len(want) {
		t.Fatalf("got %d replies, want %d", len(responses), len(want))
	}
	if len(messages) != len(want) {
		t.Fatal("reply expectations do not match requests")
	}
	for index := 0; index < len(responses) && index < len(want) && index < len(messages); index++ {
		response := responses[index]
		status := want[index]
		if response.Header.Status != status || response.Header.Flags&wire.FlagAsync != 0 || response.Header.MessageID != messages[index].Header.MessageID {
			t.Fatalf("reply %d: %+v, want status %#x and a synchronous reply", index, response.Header, status)
		}
		if status == smb.StatusSuccess && response.Header.Command == wire.Lock {
			if _, err := wire.DecodeLockResponse(response); err != nil {
				t.Fatal(err)
			}
		} else if status != smb.StatusSuccess {
			if _, err := wire.DecodeErrorResponse(response); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// From logging_test.go.
type logEntry struct {
	message string
	level   slog.Level
}

// From logging_test.go.
type recordedHandler struct {
	entries []logEntry
	mu      sync.Mutex
}

// From logging_test.go.
func (*recordedHandler) Enabled(context.Context, slog.Level) bool { return true }

// From logging_test.go.
func (handler *recordedHandler) Handle(_ context.Context, record slog.Record) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.entries = append(handler.entries, logEntry{message: record.Message, level: record.Level})
	return nil
}

// From logging_test.go.
func (handler *recordedHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }

// From logging_test.go.
func (handler *recordedHandler) WithGroup(string) slog.Handler { return handler }

// From logging_test.go.
func (handler *recordedHandler) snapshot() []logEntry {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]logEntry(nil), handler.entries...)
}

// From namespace_test.go.
type lookupStorage struct {
	lookup func(context.Context, string) (smb.Resolved, error)
	cleanupStorage
}

// From namespace_test.go.
func (storage *lookupStorage) Lookup(ctx context.Context, path string) (smb.Resolved, error) {
	return storage.lookup(ctx, path)
}

// From namespace_test.go.
func waitParentUsers(t *testing.T, server *Server, parent smb.Inode, count uint64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		server.namespaceMu.Lock()
		guard := server.parents[parent]
		ready := guard != nil && guard.refs == count
		server.namespaceMu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline:
			t.Fatal("parent users did not arrive")
		case <-time.After(time.Millisecond):
		}
	}
}

// From namespace_test.go.
type removingStorage struct {
	entered chan struct{}
	proceed chan struct{}
	cleanupStorage
}

// From namespace_test.go.
func (storage *removingStorage) Remove(ctx context.Context, name smb.Name, inode smb.Inode) error {
	close(storage.entered)
	<-storage.proceed
	return storage.cleanupStorage.Remove(ctx, name, inode)
}

// From negotiate_test.go.
func negotiateMessage(t testing.TB, credit uint16) wire.Message {
	t.Helper()
	preauth, err := wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}})
	if err != nil {
		t.Fatal(err)
	}
	encryption, err := wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM}})
	if err != nil {
		t.Fatal(err)
	}
	signing, err := wire.EncodeSigningContext(wire.SigningContext{Algorithms: []uint16{smb.SigningCMAC, smb.SigningGMAC}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeNegotiateRequest(wire.NegotiateRequest{
		Dialects: []uint16{smb.Dialect311}, SecurityMode: smb.AdvertisedSecurityMode,
		Capabilities: smb.CapabilityLargeMTU, Contexts: []wire.NegotiateContext{preauth, encryption, signing},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.Negotiate, Credit: credit}, Body: body}
}

// From negotiation_state_test.go.
func assertNegotiateFields(t *testing.T, response wire.NegotiateResponse, options Options) {
	t.Helper()
	now, err := wire.EncodeFiletime(options.Now())
	if err != nil {
		t.Fatal(err)
	}
	if response.SecurityMode != smb.AdvertisedSecurityMode || response.ServerGUID != options.ServerGUID || response.Capabilities != smb.AdvertisedCapabilities ||
		response.MaxRead != smb.MaxReadSize || response.MaxWrite != smb.MaxWriteSize || response.MaxTransact != smb.MaxTransactSize || response.SystemTime != uint64(now) {
		t.Fatalf("server negotiation fields: %+v", response)
	}
}

// From negotiation_state_test.go.
func smb1Frame(t *testing.T, dialects string) []byte {
	t.Helper()
	if len(dialects) > 65535 {
		t.Fatal("SMB1 dialect fixture is too long")
		return nil
	}
	payload := make([]byte, 35)
	copy(payload, []byte{0xff, 'S', 'M', 'B', 0x72})
	binary.LittleEndian.PutUint16(payload[33:], uint16(len(dialects)&0xffff))
	payload = append(payload, dialects...)
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	return append(frame, payload...)
}

// From negotiation_state_test.go.
func sendOpeningWildcard(ctx context.Context, t *testing.T, client *smbtest.Client) {
	t.Helper()
	if err := client.SendRaw(ctx, smb1Frame(t, "\x02SMB 2.???\x00")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err != nil {
		t.Fatal(err)
	}
}

// From object_id_regression_test.go.
const objectIDReadAttributes uint32 = 0x00000080

// From object_id_regression_test.go.
func objectIDOpenCases() []struct {
	name    string
	request wire.CreateRequest
} {
	return []struct {
		name    string
		request wire.CreateRequest
	}{
		{name: "file", request: wire.CreateRequest{Name: "object-id-test", DesiredAccess: objectIDReadAttributes, ShareAccess: 7, Disposition: fileOpenIf, Options: fileNonDirectoryFile}},
		{name: "root", request: wire.CreateRequest{Name: "", DesiredAccess: objectIDReadAttributes, ShareAccess: 7, Disposition: fileOpen, Options: fileDirectoryFile}},
	}
}

// From object_id_regression_test.go.
func objectIDAssertEcho(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64) {
	t.Helper()
	response := exchange(ctx, t, client, sessionEcho(t, session, id))[0]
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("ECHO status = %#x", response.Header.Status)
	}
	if _, err := wire.DecodeEchoResponse(response); err != nil {
		t.Fatal(err)
	}
}

// From opens_test.go.
func openRequestContext(server *Server, open state.Open) RequestContext {
	return RequestContext{
		server: server, Storage: server.options.Storage, Opens: server.options.State,
		Session: Session{SessionID: open.Binding.SessionID}, Tree: Tree{TreeID: open.Binding.TreeID},
	}
}

// From opens_test.go.
func assertCleanupWaiting(t *testing.T, done <-chan error, storage *cleanupStorage) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("cleanup finished with active references: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if storage.closed.Load() != 0 {
		t.Fatal("storage handle closed with active references")
	}
}

// From pending_io_test.go.
// Use the shared file-backed fixture behind a real S3 client and fault proxy.
// GET uses FileServer's range support; PUT stores the bytes in the same backend.
func newPendingIOFixture(t *testing.T, outage bool) (*ioFixture, *s3fault.Proxy) {
	t.Helper()
	var proxy *s3fault.Proxy
	window := 100 * time.Millisecond
	if outage {
		window = 0
	}
	fixture := newConfiguredIOFixture(t, nil, func(dir string, mc *meta.Config, cc *chunk.Config, store *ioStore) {
		backendStore := store.ObjectStorage
		files := http.StripPrefix("/bucket/", http.FileServer(http.Dir(filepath.Join(dir, "objects"))))
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				files.ServeHTTP(w, r)
				return
			}
			key := strings.TrimPrefix(r.URL.Path, "/bucket/")
			if err := backendStore.Put(r.Context(), key, r.Body); err != nil {
				t.Errorf("S3 fixture PUT: %v", err)
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(backend.Close)
		var err error
		proxy, err = s3fault.New(t.Context(), backend.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := proxy.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		store.ObjectStorage, err = object.CreateStorage("s3", proxy.URL()+"/bucket", "test-access", "test-secret", "")
		if err != nil {
			t.Fatal(err)
		}
		mc.Retries = 2
		if outage {
			mc.Retries, cc.MaxRetries = 4, 3
			if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
				// Match the production retry budgets and block timeouts in
				// internal/storage/runtime.go, but keep the cache cold.
				mc.Retries, cc.MaxRetries = 53, 12
				cc.GetTimeout, cc.PutTimeout = time.Minute, time.Minute
				store.put = func(ctx context.Context, key string, src io.Reader) error {
					start := time.Now()
					putErr := store.ObjectStorage.Put(ctx, key, src)
					t.Logf("S3 PUT %s at %s: duration %s, error %v", key, start.Format(time.RFC3339Nano), time.Since(start), putErr)
					return putErr
				}
			}
		}
	}, window)
	return fixture, proxy
}

// From pending_io_test.go.
func pendingClient(t *testing.T, fixture *ioFixture, bound time.Duration) (*Server, *smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := boundedPipeClient(t, server, false, bound)
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return server, client, ctx, session
}

// From pending_io_test.go.
func pendingRead(t *testing.T, session smbtest.Session, id uint64, open state.Open, offset uint64, length uint32) wire.Message {
	t.Helper()
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, Offset: offset, Length: length})
	if err != nil {
		t.Fatal(err)
	}
	charge := pendingCharge(t, length)
	return ioMessage(session, id, wire.Read, body, charge)
}

// From pending_io_test.go.
func pendingWrite(t *testing.T, session smbtest.Session, id uint64, open state.Open, data []byte) wire.Message {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, Data: data, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	charge := pendingCharge(t, pendingLength(t, data))
	return ioMessage(session, id, wire.Write, body, charge)
}

// From pending_io_test.go.
func pendingLength(t *testing.T, data []byte) uint32 {
	t.Helper()
	length := len(data)
	if length < 0 || length > math.MaxUint32 {
		t.Fatal("I/O test payload is too large")
		return 0
	}
	return uint32(length)
}

// From pending_io_test.go.
func pendingCharge(t *testing.T, length uint32) uint16 {
	t.Helper()
	charge := (uint64(length) + uint64(smb.CreditUnit) - 1) / uint64(smb.CreditUnit)
	if charge > 16 {
		t.Fatal("I/O test payload is too large")
		return 0
	}
	return uint16(charge)
}

// From pending_io_test.go.
func receivePendingIO(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply: %+v", response.Messages)
	}
	return response.Messages[0]
}

// From pending_io_test.go.
func assertInterimIO(t *testing.T, request, interim wire.Message) {
	t.Helper()
	if interim.Header.Status != smb.StatusPending || interim.Header.Flags&wire.FlagAsync == 0 || interim.Header.AsyncID == 0 || interim.Header.MessageID != request.Header.MessageID || interim.Header.SessionID != request.Header.SessionID || interim.Header.Command != request.Header.Command || interim.Header.Credit != request.Header.Credit {
		t.Fatalf("invalid interim reply: %+v for %+v", interim.Header, request.Header)
	}
	if _, err := wire.DecodeErrorResponse(interim); err != nil {
		t.Fatal(err)
	}
}

// From pending_io_test.go.
func assertFinalIO(t *testing.T, interim, final wire.Message, status smb.Status) {
	t.Helper()
	if final.Header.Status != status || final.Header.Flags&wire.FlagAsync == 0 || final.Header.AsyncID != interim.Header.AsyncID || final.Header.MessageID != interim.Header.MessageID || final.Header.SessionID != interim.Header.SessionID || final.Header.Command != interim.Header.Command || final.Header.Credit != 0 {
		t.Fatalf("invalid final reply: %+v after %+v", final.Header, interim.Header)
	}
}

// From pending_io_test.go.
func seedPendingData(ctx context.Context, t *testing.T, fixture *ioFixture, open state.Open, data []byte) {
	t.Helper()
	if n, err := fixture.adapter.WriteAt(ctx, open.Handle, data, 0); err != nil || n != len(data) {
		t.Fatalf("seed write: %d, %v", n, err)
	}
	if err := fixture.adapter.Flush(ctx, open.Handle, smb.SyncData); err != nil {
		t.Fatal(err)
	}
}

// From pending_io_test.go.
func warmPendingRead(ctx context.Context, t *testing.T, fixture *ioFixture, open state.Open, data []byte) {
	t.Helper()
	got := make([]byte, len(data))
	if n, err := fixture.adapter.ReadAt(ctx, open.Handle, got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("warm read: %d, %q, %v", n, got, err)
	}
}

// From pending_io_test.go.
func assertUnrelatedIO(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, cached state.Open, next uint64, data []byte) {
	t.Helper()
	// Both replies must arrive promptly while the storage request is pending.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	echoRequest := sessionEcho(t, session, next)
	response := ioRoundTrip(ctx, t, client, echoRequest)
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	response = ioRoundTrip(ctx, t, client, pendingRead(t, session, next+1, cached, 0, pendingLength(t, data)))
	assertReadIO(t, response, data)
}

// From pending_io_test.go.
func assertReadIO(t *testing.T, response wire.Message, data []byte) {
	t.Helper()
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	result, err := wire.DecodeReadResponse(response)
	if err != nil || !bytes.Equal(result.Data, data) {
		t.Fatalf("read data: %q, %v", result.Data, err)
	}
}

// From pending_io_test.go.
func assertIOSuccess(t *testing.T, command wire.Command, final wire.Message, data []byte) {
	t.Helper()
	switch uint16(command) {
	case uint16(wire.Read):
		assertReadIO(t, final, data)
	case uint16(wire.Write):
		result, err := wire.DecodeWriteResponse(final)
		if err != nil || int(result.Count) != len(data) {
			t.Fatalf("WRITE result: %+v, %v", result, err)
		}
	case uint16(wire.Flush):
		if _, err := wire.DecodeFlushResponse(final); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unexpected I/O command")
	}
}

// From pending_io_test.go.
func assertOutageStarted(t *testing.T, proxy *s3fault.Proxy, start time.Time, command wire.Command) {
	t.Helper()
	paths := make(map[string]bool)
	count, method := 1, http.MethodPut
	if command == wire.Read {
		count, method = 2, http.MethodGet
	}
	timer := time.NewTimer(time.Until(start.Add(time.Second)))
	defer timer.Stop()
	for len(paths) < count {
		select {
		case event := <-proxy.OutageSeen():
			if event.Method != method || event.Status != http.StatusServiceUnavailable || time.Since(start) >= time.Second {
				t.Fatalf("unexpected outage request: %+v after %s", event, time.Since(start))
			}
			paths[event.Path] = true
		case <-timer.C:
			t.Fatal("cold I/O did not reach failed S3 in the first second")
		}
	}
}

// From previous_session_test.go.
func assertRawStatus(ctx context.Context, t *testing.T, client *smbtest.Client, message wire.Message, status smb.Status) {
	t.Helper()
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Header.Status != status {
		t.Fatalf("raw status: %+v, want %#x", members, status)
	}
}

// From protection_denial_test.go.
func checkPlaintextDenial(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector, response []byte, receiveErr error, message wire.Message, calls *atomic.Int32) {
	t.Helper()
	if receiveErr != nil {
		t.Fatal(receiveErr)
	}
	plain, err := protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Header.Status != smb.StatusAccessDenied || calls.Load() != 0 {
		t.Fatal("plaintext did not get ACCESS_DENIED before dispatch")
	}
	message.Header.MessageID++
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	payload, err = protector.Seal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(ctx, t, client, payload)
	response, err = client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err = wire.Split(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Header.Status != smb.StatusSuccess || calls.Load() != 1 {
		t.Fatal("plaintext refusal closed the connection")
	}
}

// From protection_handoff_test.go.
func checkProtectionHandoff(t *testing.T, encrypted, required, denied bool) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = 99
	protection := crypt.Options{SessionKey: []byte("0123456789abcdef"), SessionID: sessionID, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC}
	serverKey, err := crypt.NewProtector(protection)
	if err != nil {
		t.Fatal(err)
	}
	protection.Role = crypt.RoleClient
	clientKey, err := crypt.NewProtector(protection)
	if err != nil {
		t.Fatal(err)
	}
	owner := newConnection(t.Context(), func() {}, server, nil)
	owner.sessions[sessionID] = &sessionEntry{active: true, protector: serverKey, identity: Session{SessionID: sessionID, User: options.Account.User, Encrypted: required}}
	server.registerSession(sessionID, owner)
	requests := []wire.Message{sessionEcho(t, smbtest.Session{SessionID: sessionID}, 1), sessionEcho(t, smbtest.Session{SessionID: sessionID}, 2)}
	payload, err := wire.Join(requests)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted {
		payload, err = clientKey.Seal(payload)
	} else {
		payload = signMessages(t, clientKey, requests...)
		if denied && !required {
			payload[48] ^= 1
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	messages, err := owner.decodePayload(payload)
	if denied && !errors.Is(err, errAccessDenied) || !denied && err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Complete a replacement on another connection at the old receive loop's
	// verification-to-retention handoff, before any member is dispatched.
	client, ctx := pipeClient(t, server)
	fresh, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}, PreviousSessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionID == sessionID {
		t.Fatal("replacement reused identity")
	}
	var responses []wire.Message
	status := smb.StatusUserSessionDeleted
	if denied {
		status = smb.StatusAccessDenied
	}
	for _, message := range messages {
		result := reply{status: status}
		if !denied {
			result = owner.execute(ctx, message, compoundState{})
		}
		response, responseErr := makeResponse(message.Header, result, 1)
		if responseErr != nil {
			t.Fatal(responseErr)
		}
		responses = append(responses, response)
	}
	response, err := owner.encodePayload(responses)
	if err != nil {
		t.Fatal(err)
	}
	checkRetainedReply(t, clientKey, response, encrypted || required, status, len(requests))
	if len(owner.replyProtection) != 0 {
		t.Fatal("final replies retained keys")
	}
	if result := owner.execute(ctx, requests[0], compoundState{}); result.status != smb.StatusUserSessionDeleted {
		t.Fatal("saved reply key restored removed identity")
	}
	if response := exchange(ctx, t, client, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("replacement identity is unusable")
	}
}

// From protection_handoff_test.go.
func checkRetainedReply(t *testing.T, clientKey *crypt.Protector, response []byte, encrypted bool, status smb.Status, count int) {
	t.Helper()
	if encrypted {
		plain, err := clientKey.Open(response)
		if err != nil {
			t.Fatalf("reply lost encryption after replacement: %v", err)
		}
		response = plain
	}
	members, err := wire.Split(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != count {
		t.Fatal("missing denial replies")
	}
	for _, member := range members {
		if member.Header.Status != status {
			t.Fatalf("reply status: %#x, want %#x", member.Header.Status, status)
		}
		if encrypted {
			if member.Header.Flags&wire.FlagSigned != 0 {
				t.Fatal("encrypted reply was separately signed")
			}
		} else if err := clientKey.Verify(member.Raw); err != nil {
			t.Fatalf("reply lost signature after replacement: %v", err)
		}
	}
}

// From protection_test.go.
// rawLogin hashes the exact handshake independently of crypt.Preauth and the
// client's Login. It leaves subsequent protection under the test's control.
func rawLogin(t *testing.T, server *Server) (*smbtest.Client, context.Context, uint64, *crypt.Protector) {
	t.Helper()
	client, ctx := pipeClient(t, server)
	var transcript crypt.PreauthHash
	update := func(payload []byte) { transcript = sha512.Sum512(append(transcript[:], payload...)) }
	request := negotiateMessage(t, 16)
	raw, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	update(raw)
	response := exchange(ctx, t, client, request)[0]
	update(response.Raw)
	negotiated, err := wire.DecodeNegotiateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	initiator, err := auth.NewInitiator(server.options.Account, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initiator.Start(negotiated.Token)
	if err != nil {
		t.Fatal(err)
	}
	id := uint64(0)
	var key []byte
	for messageID := uint64(1); messageID <= 2; messageID++ {
		body, encodeErr := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, SecurityMode: 3})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		request = wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: messageID, SessionID: id, CreditCharge: 1, Credit: 16}, Body: body}
		raw, err = wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		update(raw)
		response = exchange(ctx, t, client, request)[0]
		id = response.Header.SessionID
		if response.Header.Credit < 5 {
			t.Fatal("setup did not grant five credits")
		}
		setup, decodeErr := wire.DecodeSessionSetupResponse(response)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if messageID == 1 {
			if response.Header.Status != smb.StatusMoreProcessingRequired {
				t.Fatalf("challenge: %+v", response.Header)
			}
			update(response.Raw)
		} else if response.Header.Status != smb.StatusSuccess || response.Header.Flags&wire.FlagSigned == 0 {
			t.Fatalf("final setup: %+v", response.Header)
		}
		result, err = initiator.Step(setup.Token)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.SessionKey) != 0 {
			key = result.SessionKey
		}
	}
	options := crypt.Options{SessionKey: key, Preauth: transcript, SessionID: id, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, Role: crypt.RoleClient}
	protector, err := crypt.NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	if verifyErr := protector.Verify(response.Raw); verifyErr != nil {
		t.Fatalf("final signature does not match transcript: %v", verifyErr)
	}
	update(response.Raw)
	options.Preauth = transcript
	wrong, err := crypt.NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.Verify(response.Raw); err == nil {
		t.Fatal("final response was included in key derivation")
	}
	return client, ctx, id, protector
}

// From protection_test.go.
func signMessages(t *testing.T, protector *crypt.Protector, messages ...wire.Message) []byte {
	t.Helper()
	for index := range messages {
		messages[index].Header.Flags |= wire.FlagSigned
	}
	payload, err := wire.Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	for _, member := range members {
		signature, err := protector.Sign(member.Raw)
		if err != nil {
			t.Fatal(err)
		}
		copy(payload[offset+48:offset+64], signature[:])
		offset += len(member.Raw)
	}
	return payload
}

// From protection_test.go.
func sendPayload(ctx context.Context, t *testing.T, client *smbtest.Client, payload []byte) {
	t.Helper()
	frame := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	if err := client.SendRaw(ctx, append(frame, payload...)); err != nil {
		t.Fatal(err)
	}
}

// From protection_test.go.
func checkPlainCompound(t *testing.T, tamper int) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		calls.Add(1)
		return handleEcho(ctx, request, message)
	}
	client, ctx, id, protector := rawLogin(t, server)
	first, second := echo(t, 3), echo(t, 4)
	first.Header.SessionID = id
	second.Header.SessionID, second.Header.Flags = ^uint64(0), wire.FlagRelated
	payload := signMessages(t, protector, first, second)
	if tamper == 2 {
		payload[72+16] &^= byte(wire.FlagSigned)
	} else if tamper >= 0 {
		payload[tamper*72+48] ^= 1
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, wantCalls := smb.StatusSuccess, int32(2)
	if tamper >= 0 {
		want, wantCalls = smb.StatusAccessDenied, 0
	}
	if len(response.Messages) != 2 || calls.Load() != wantCalls {
		t.Fatalf("compound: %+v", response.Messages)
	}
	for _, member := range response.Messages {
		if member.Header.SessionID != id || member.Header.Status != want {
			t.Fatalf("related identity: %+v", member.Header)
		}
		if verifyErr := protector.Verify(member.Raw); verifyErr != nil {
			t.Fatal(verifyErr)
		}
	}
	message := echo(t, 5)
	message.Header.SessionID = id
	sendPayload(ctx, t, client, signMessages(t, protector, message))
	if next := receiveSignedEcho(ctx, t, client, protector); next.Header.Status != smb.StatusSuccess || calls.Load() != wantCalls+1 {
		t.Fatal("signature refusal closed the connection")
	}
}

// From protection_test.go.
func receiveSignedEcho(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatal("expected one ECHO reply")
	}
	if err := protector.Verify(response.Messages[0].Raw); err != nil {
		t.Fatal(err)
	}
	return response.Messages[0]
}

// From protection_test.go.
func checkEncryptionInput(t *testing.T, mode string) {
	t.Helper()
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		calls.Add(1)
		return handleEcho(ctx, request, message)
	}
	client, ctx, id, protector := rawLogin(t, server)
	message := echo(t, 3)
	message.Header.SessionID = id
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	if mode == "plaintext" {
		payload = signMessages(t, protector, message)
	} else {
		payload, err = protector.Seal(payload)
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "tag":
			payload[4] ^= 1
		case "ciphertext":
			payload[len(payload)-1] ^= 1
		case "session":
			payload[44] ^= 1
		}
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.ReceiveRaw(ctx)
	if mode == "plaintext" {
		checkPlaintextDenial(ctx, t, client, protector, response, err, message, &calls)
		return
	}
	if mode != "valid" {
		if err == nil || calls.Load() != 0 {
			t.Fatal("invalid protection reached a handler")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(plain)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || members[0].Header.Flags&wire.FlagSigned != 0 {
		t.Fatal("encrypted response was signed separately")
	}
}

// From protocol_test.go.
func assertSelectedContexts(t *testing.T, message wire.Message, wantSigning uint16) {
	t.Helper()
	response, err := wire.DecodeNegotiateResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Contexts) != 3 {
		t.Fatalf("ignored contexts were echoed: %+v", response.Contexts)
	}
	if wantSigning != 0 {
		signing, err := wire.DecodeSigningContext(response.Contexts[2])
		if err != nil || len(signing.Algorithms) != 1 || signing.Algorithms[0] != wantSigning {
			t.Fatalf("default signing: %+v, %v", signing, err)
		}
	}
}

// From query_directory_test.go.
type directoryFixture struct {
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

// From query_directory_test.go.
func newDirectoryFixture(t *testing.T) *directoryFixture {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return &directoryFixture{server: server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

// From query_directory_test.go.
func (f *directoryFixture) create(t *testing.T, name string, kind smb.Kind) smb.Resolved {
	t.Helper()
	resolved, err := f.server.options.Storage.Lookup(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = f.server.options.Storage.Create(f.ctx, resolved.Name, kind)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// From query_directory_test.go.
func (f *directoryFixture) open(t *testing.T, name string, access uint32) state.Open {
	t.Helper()
	storage := f.server.options.Storage
	resolved, err := storage.Lookup(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	reservation, status := f.server.options.State.Reserve(state.OpenRequest{Object: resolved.Object, Binding: state.Binding{SessionID: f.session.SessionID, TreeID: f.session.TreeID}, User: f.server.options.Account.User, Share: f.server.options.ShareName, GrantedAccess: access, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := storage.Open(f.ctx, resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	open, status := f.server.options.State.Commit(reservation, state.Grant{Handle: handle, Directory: resolved.Attr.Kind == smb.KindDirectory})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

// From query_directory_test.go.
func (f *directoryFixture) query(t *testing.T, open state.Open, pattern string, flags uint8, class wire.DirectoryInfoClass, length uint32) (smb.Status, []wire.DirectoryEntry) {
	t.Helper()
	query := wire.QueryDirectoryRequest{ID: wire.FileID(open.ID), Pattern: pattern, Flags: flags, InfoClass: class, OutputLength: length, FileIndex: 0xffffffff}
	body, err := wire.EncodeQueryDirectoryRequest(query)
	if err != nil {
		t.Fatal(err)
	}
	units := (uint64(length) + uint64(smb.CreditUnit) - 1) / uint64(smb.CreditUnit)
	if units > 65535 {
		t.Fatal("output length exceeds the credit charge field")
		return smb.StatusInvalidParameter, nil
	}
	charge := max(uint16(1), uint16(units))
	message := wire.Message{Header: wire.Header{Command: wire.QueryDirectory, MessageID: f.next, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: charge, Credit: 16}, Body: body}
	f.next += uint64(charge)
	response := exchange(f.ctx, t, f.client, message)[0]
	if response.Header.Status != smb.StatusSuccess {
		return response.Header.Status, nil
	}
	buffer, err := wire.DecodeQueryDirectoryResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(buffer.Data) > int(length) {
		t.Fatalf("reply length %d exceeds %d", len(buffer.Data), length)
	}
	entries := decodeDirectoryTestEntries(t, class, buffer.Data)
	for _, entry := range entries {
		if entry.Metadata.FileIndex != 0 {
			t.Fatal("server advertised a directory index")
		}
	}
	return response.Header.Status, entries
}

// From query_directory_test.go.
func decodeDirectoryTestEntries(t *testing.T, class wire.DirectoryInfoClass, data []byte) []wire.DirectoryEntry {
	t.Helper()
	var entries []wire.DirectoryEntry
	var err error
	switch uint8(class) {
	case uint8(wire.ClassDirectory):
		entries, err = wire.DecodeDirectoryEntries(data)
	case uint8(wire.ClassDirectoryFull):
		entries, err = wire.DecodeDirectoryFullEntries(data)
	case uint8(wire.ClassDirectoryBoth):
		entries, err = wire.DecodeDirectoryBothEntries(data)
	case uint8(wire.ClassDirectoryNames):
		entries, err = wire.DecodeDirectoryNamesEntries(data)
	case uint8(wire.ClassDirectoryIDBoth):
		var decoded []wire.DirectoryIDBothEntry
		decoded, err = wire.DecodeDirectoryIDBothEntries(data)
		for _, entry := range decoded {
			entries = append(entries, wire.DirectoryEntry(entry))
		}
	case uint8(wire.ClassDirectoryIDFull):
		var decoded []wire.DirectoryIDFullEntry
		decoded, err = wire.DecodeDirectoryIDFullEntries(data)
		for _, entry := range decoded {
			entries = append(entries, wire.DirectoryEntry{Name: entry.Name, Metadata: entry.Metadata})
		}
	default:
		t.Fatalf("unsupported test class %d", class)
	}
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// From query_directory_test.go.
func directoryNames(entries []wire.DirectoryEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names
}

// From query_info_test.go.
type queryInfoFixture struct {
	storage smb.Storage
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

// From query_info_test.go.
func newQueryInfoFixture(t *testing.T) *queryInfoFixture {
	t.Helper()
	return newQueryInfoFixtureWithStorage(t, newFilesMetaStorage(t))
}

// From query_info_test.go.
func (f *queryInfoFixture) create(t *testing.T, path string, kind smb.Kind) (smb.Resolved, state.Open) {
	t.Helper()
	resolved, err := f.storage.Lookup(f.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = f.storage.Create(f.ctx, resolved.Name, kind)
	if err != nil {
		t.Fatal(err)
	}
	return resolved, f.open(t, resolved.Object)
}

// From query_info_test.go.
func (f *queryInfoFixture) open(t *testing.T, object smb.ObjectKey) state.Open {
	t.Helper()
	table := f.server.options.State
	request := state.OpenRequest{Object: object, Binding: state.Binding{SessionID: f.session.SessionID, TreeID: f.session.TreeID}, User: f.server.options.Account.User, Share: f.server.options.ShareName, GrantedAccess: 0x001f01ff, Sharing: 7}
	reservation, status := table.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := f.storage.Open(f.ctx, object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		if abortStatus := table.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(err)
	}
	open, status := table.Commit(reservation, state.Grant{Handle: handle})
	if status != smb.StatusSuccess {
		if err := f.storage.Close(f.ctx, handle); err != nil {
			t.Error(err)
		}
		if abortStatus := table.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(status)
	}
	return open
}

// From query_info_test.go.
func (f *queryInfoFixture) query(t *testing.T, open state.Open, infoType wire.InfoType, class uint8, length uint32) wire.Message {
	t.Helper()
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, InfoType: infoType, InfoClass: class, OutputLength: length})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.QueryInfo, MessageID: f.next, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: 1, Credit: 16}, Body: body}
	f.next++
	return exchange(f.ctx, t, f.client, message)[0]
}

// From query_info_test.go.
func queryData(t *testing.T, message wire.Message, status smb.Status) []byte {
	t.Helper()
	if message.Header.Status != status {
		t.Fatalf("status = %#x, want %#x", message.Header.Status, status)
	}
	response, err := wire.DecodeQueryInfoResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	return response.Data
}

// From query_info_test.go.
func decodeQueryClass[T any](t *testing.T, data []byte, decode func([]byte) (T, error), want T) {
	t.Helper()
	got, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}

// From query_info_test.go.
func expectedQueryBasic(t *testing.T, attr smb.Attr) wire.FileBasicInformation {
	t.Helper()
	convert := func(value time.Time) wire.Filetime {
		t.Helper()
		result, err := wire.EncodeFiletime(value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	return wire.FileBasicInformation{Created: convert(attr.Created), Accessed: convert(attr.Accessed), Modified: convert(attr.Modified), Changed: convert(attr.Changed), Attributes: attr.Attributes}
}

// From query_info_test.go.
var queryFileClasses = []wire.FileInfoClass{
	wire.ClassFileBasic, wire.ClassFileStandard, wire.ClassFileInternal, wire.ClassFileEA,
	wire.ClassFileAccess, wire.ClassFilePosition, wire.ClassFileMode, wire.ClassFileAlignment,
	wire.ClassFileName, wire.ClassFileAll, wire.ClassFileNetworkOpen, wire.ClassFileAttributeTag,
	wire.ClassFileStream, wire.ClassFileID,
}

// From query_info_test.go.
func checkQueryClass(t *testing.T, f *queryInfoFixture, open state.Open, attr smb.Attr, path string, class wire.FileInfoClass, data []byte) {
	t.Helper()
	basic := expectedQueryBasic(t, attr)
	standard := wire.FileStandardInformation{AllocationSize: attr.AllocationSize, EndOfFile: attr.Size, Links: 1, Directory: attr.Kind == smb.KindDirectory}
	switch class {
	case wire.ClassFileBasic:
		decodeQueryClass(t, data, wire.DecodeFileBasicInformation, basic)
	case wire.ClassFileStandard:
		decodeQueryClass(t, data, wire.DecodeFileStandardInformation, standard)
	case wire.ClassFileInternal:
		decodeQueryClass(t, data, wire.DecodeFileInternalInformation, wire.FileInternalInformation{Index: uint64(attr.Inode)})
	case wire.ClassFileEA:
		decodeQueryClass(t, data, wire.DecodeFileEAInformation, wire.FileEAInformation{})
	case wire.ClassFileAccess:
		decodeQueryClass(t, data, wire.DecodeFileAccessInformation, wire.FileAccessInformation{Access: open.GrantedAccess})
	case wire.ClassFilePosition:
		decodeQueryClass(t, data, wire.DecodeFilePositionInformation, wire.FilePositionInformation{})
	case wire.ClassFileMode:
		decodeQueryClass(t, data, wire.DecodeFileModeInformation, wire.FileModeInformation{})
	case wire.ClassFileAlignment:
		decodeQueryClass(t, data, wire.DecodeFileAlignmentInformation, wire.FileAlignmentInformation{})
	case wire.ClassFileName:
		decodeQueryClass(t, data, wire.DecodeFileNameInformation, wire.FileNameInformation{Name: path})
	case wire.ClassFileAll:
		decodeQueryClass(t, data, wire.DecodeFileAllInformation, wire.FileAllInformation{Basic: basic, Standard: standard, Internal: wire.FileInternalInformation{Index: uint64(attr.Inode)}, Access: wire.FileAccessInformation{Access: open.GrantedAccess}, Name: wire.FileNameInformation{Name: path}})
	case wire.ClassFileNetworkOpen:
		decodeQueryClass(t, data, wire.DecodeFileNetworkOpenInformation, wire.FileNetworkOpenInformation{Created: basic.Created, Accessed: basic.Accessed, Modified: basic.Modified, Changed: basic.Changed, AllocationSize: attr.AllocationSize, EndOfFile: attr.Size, Attributes: attr.Attributes})
	case wire.ClassFileAttributeTag:
		decodeQueryClass(t, data, wire.DecodeFileAttributeTagInformation, wire.FileAttributeTagInformation{Attributes: attr.Attributes})
	case wire.ClassFileStream:
		want := wire.FileStreamInformation{}
		if attr.Kind == smb.KindFile {
			want.Entries = append(want.Entries, wire.FileStreamEntry{Name: "::$DATA", Size: attr.Size, AllocationSize: attr.AllocationSize})
		}
		decodeQueryClass(t, data, wire.DecodeFileStreamInformation, want)
	case wire.ClassFileID:
		space, err := f.storage.StatFS(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := wire.FileIDInformation{VolumeSerial: space.VolumeID}
		binary.LittleEndian.PutUint64(want.ID[:8], uint64(attr.Inode))
		decodeQueryClass(t, data, wire.DecodeFileIDInformation, want)
	case wire.ClassFileRename, wire.ClassFileDisposition, wire.ClassFileAllocation, wire.ClassFileEndOfFile:
		t.Fatalf("unsupported test class %d", class)
	default:
		t.Fatalf("missing test for class %d", class)
	}
}

// From query_info_test.go.
func checkQueryPrefix(t *testing.T, f *queryInfoFixture, open state.Open, class wire.FileInfoClass, length uint32, full []byte) {
	t.Helper()
	wantStatus := smb.StatusSuccess
	if uint64(length) < uint64(len(full)) {
		wantStatus = smb.StatusBufferOverflow
	}
	got := queryData(t, f.query(t, open, wire.InfoFile, uint8(class), length), wantStatus)
	if !bytes.Equal(got, full[:length]) {
		t.Fatalf("length %d: truncated output = %x, want %x", length, got, full[:length])
	}
}

// From query_info_test.go.
func (f *queryInfoFixture) echo(t *testing.T) {
	t.Helper()
	message := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, f.next))[0]
	f.next++
	if message.Header.Status != smb.StatusSuccess {
		t.Fatalf("ECHO after query: %#x", message.Header.Status)
	}
}

// From raw_session_helpers_test.go.
// rawSessions permits repeated exchanges on one transport. Client.Login is
// deliberately a one-shot helper, so lifecycle tests control the transcript,
// message IDs and protection explicitly here.
type rawSessions struct {
	ctx     context.Context
	client  *smbtest.Client
	server  *Server
	preauth *crypt.Preauth
	token   []byte
	nextID  uint64
}

// From raw_session_helpers_test.go.
type rawAuthentication struct {
	preauth *crypt.Preauth
	result  auth.Result
	id      uint64
	flags   uint8
}

// From raw_session_helpers_test.go.
func newRawSessions(t *testing.T, server *Server) *rawSessions {
	t.Helper()
	client, ctx := pipeClient(t, server)
	request := negotiateMessage(t, 16)
	preauth := crypt.NewPreauth()
	payload, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	preauth.Update(payload)
	response := exchange(ctx, t, client, request)[0]
	preauth.Update(response.Raw)
	negotiated, err := wire.DecodeNegotiateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return &rawSessions{ctx: ctx, client: client, server: server, preauth: preauth, token: negotiated.Token, nextID: 1}
}

// From raw_session_helpers_test.go.
func (client *rawSessions) start(t *testing.T, flags uint8) rawAuthentication {
	t.Helper()
	initiator, err := auth.NewInitiator(client.server.options.Account, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initiator.Start(client.token)
	if err != nil {
		t.Fatal(err)
	}
	preauth := client.preauth.Fork()
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, Flags: flags, SecurityMode: 3})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: client.nextID, CreditCharge: 1, Credit: 16}, Body: body}
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	preauth.Update(payload)
	response := exchange(client.ctx, t, client.client, message)[0]
	client.nextID++
	if response.Header.Status != smb.StatusMoreProcessingRequired {
		t.Fatalf("challenge: %+v", response.Header)
	}
	preauth.Update(response.Raw)
	setup, err := wire.DecodeSessionSetupResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	result, err = initiator.Step(setup.Token)
	if err != nil {
		t.Fatal(err)
	}
	return rawAuthentication{preauth: preauth, result: result, id: response.Header.SessionID, flags: flags}
}

// From raw_session_helpers_test.go.
func (client *rawSessions) finish(t *testing.T, authentication rawAuthentication, previousID uint64) *crypt.Protector {
	t.Helper()
	message := client.continuation(t, authentication, previousID)
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	authentication.preauth.Update(payload)
	response := exchange(client.ctx, t, client.client, message)[0]
	client.nextID++
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("authentication: %+v", response.Header)
	}
	protector, err := crypt.NewProtector(crypt.Options{SessionKey: authentication.result.SessionKey, Preauth: authentication.preauth.Sum(), SessionID: authentication.id, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, Role: crypt.RoleClient})
	if err != nil {
		t.Fatal(err)
	}
	if err := protector.Verify(response.Raw); err != nil {
		t.Fatal(err)
	}
	return protector
}

// From raw_session_helpers_test.go.
func (client *rawSessions) continuation(t *testing.T, authentication rawAuthentication, previousID uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: authentication.result.Token, Flags: authentication.flags, SecurityMode: 3, PreviousSessionID: previousID})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: client.nextID, SessionID: authentication.id, CreditCharge: 1, Credit: 16}, Body: body}
}

// From raw_session_helpers_test.go.
func (client *rawSessions) protected(t *testing.T, protector *crypt.Protector, message wire.Message) wire.Message {
	t.Helper()
	message.Header.MessageID = client.nextID
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	payload, err = protector.Seal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(client.ctx, t, client.client, payload)
	response, err := client.client.ReceiveRaw(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	client.nextID++
	payload, err = protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatal("expected one protected reply")
	}
	return members[0]
}

// From read_write_test.go.
type readWriteClient struct {
	client  *smbtest.Client
	server  *Server
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

// From read_write_test.go.
func newReadWriteClient(t *testing.T, storage smb.Storage) *readWriteClient {
	t.Helper()
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	// Grow the window before exercising maximum-size multi-credit operations.
	message := sessionEcho(t, session, session.NextMessageID)
	message.Header.Credit = 32
	if response := ioRoundTrip(ctx, t, client, message); response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	return &readWriteClient{client: client, server: server, ctx: ctx, session: session, next: session.NextMessageID + 1}
}

// From read_write_test.go.
func (client *readWriteClient) read(t *testing.T, request wire.ReadRequest, charge uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeReadRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return client.exchange(t, wire.Read, body, charge)
}

// From read_write_test.go.
func (client *readWriteClient) write(t *testing.T, request wire.WriteRequest, charge uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeWriteRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return client.exchange(t, wire.Write, body, charge)
}

// From read_write_test.go.
func (client *readWriteClient) exchange(t *testing.T, command wire.Command, body []byte, charge uint16) wire.Message {
	t.Helper()
	message := ioMessage(client.session, client.next, command, body, charge)
	client.next += uint64(max(charge, 1))
	return ioRoundTrip(client.ctx, t, client.client, message)
}

// From read_write_test.go.
func requireIOStatus(t *testing.T, response wire.Message, want smb.Status) {
	t.Helper()
	if response.Header.Status != want {
		t.Fatalf("status %#x, want %#x", response.Header.Status, want)
	}
}

// From read_write_test.go.
type writeBarrier struct {
	entered, resume chan struct{}
	once            sync.Once
	full            bool
}

// From read_write_test.go.
func (barrier *writeBarrier) Commit(ctx context.Context, full bool) error {
	barrier.once.Do(func() { barrier.full = full; close(barrier.entered) })
	select {
	case <-barrier.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// From read_write_test.go.
type failedIOStorage struct {
	smb.Storage
	readErr, writeErr, flushErr error
	readCount, writeCount       int
	flushes                     int
}

// From read_write_test.go.
func (storage *failedIOStorage) ReadAt(_ context.Context, _ smb.Handle, data []byte, _ uint64) (int, error) {
	copy(data, "data")
	return storage.readCount, storage.readErr
}

// From read_write_test.go.
func (storage *failedIOStorage) WriteAt(_ context.Context, _ smb.Handle, _ []byte, _ uint64) (int, error) {
	return storage.writeCount, storage.writeErr
}

// From read_write_test.go.
func (storage *failedIOStorage) Flush(_ context.Context, _ smb.Handle, _ smb.SyncMode) error {
	storage.flushes++
	return storage.flushErr
}

// From reconnect_netfault_test.go.
// The gate holds exactly one request at the real adapter boundary. Cutting its
// transport cancels that request, without guessing when TCP traffic arrived.
type reconnectGate struct {
	entered  chan struct{}
	canceled chan struct{}
	command  wire.Command
}

// From reconnect_netfault_test.go.
type reconnectStorage struct {
	smb.Storage
	gate    *reconnectGate
	closed  chan struct{}
	removed chan struct{}
	mu      sync.Mutex
}

// From reconnect_netfault_test.go.
// observeCleanup reports the selected band's storage cleanup, not merely its
// removal from the open table. Expiry dispatches that work asynchronously.
func (storage *reconnectStorage) observeCleanup() (<-chan struct{}, <-chan struct{}) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	storage.closed = make(chan struct{})
	storage.removed = make(chan struct{})
	return storage.closed, storage.removed
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) Close(ctx context.Context, handle smb.Handle) error {
	if err := storage.Storage.Close(ctx, handle); err != nil {
		return err
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.closed != nil {
		close(storage.closed)
		storage.closed = nil
	}
	return nil
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) Remove(ctx context.Context, name smb.Name, inode smb.Inode) error {
	if err := storage.Storage.Remove(ctx, name, inode); err != nil {
		return err
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.removed != nil {
		close(storage.removed)
		storage.removed = nil
	}
	return nil
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) arm(command wire.Command) *reconnectGate {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	gate := &reconnectGate{command: command, entered: make(chan struct{}), canceled: make(chan struct{})}
	storage.gate = gate
	return gate
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) wait(ctx context.Context, command wire.Command) error {
	storage.mu.Lock()
	gate := storage.gate
	if gate == nil || gate.command != command {
		storage.mu.Unlock()
		return nil
	}
	storage.gate = nil
	storage.mu.Unlock()
	close(gate.entered)
	<-ctx.Done()
	close(gate.canceled)
	return ctx.Err()
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) ReadAt(ctx context.Context, handle smb.Handle, dst []byte, offset uint64) (int, error) {
	if err := storage.wait(ctx, wire.Read); err != nil {
		return 0, err
	}
	return storage.Storage.ReadAt(ctx, handle, dst, offset)
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) WriteAt(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
	if err := storage.wait(ctx, wire.Write); err != nil {
		return 0, err
	}
	return storage.Storage.WriteAt(ctx, handle, src, offset)
}

// From reconnect_netfault_test.go.
func (storage *reconnectStorage) Flush(ctx context.Context, handle smb.Handle, mode smb.SyncMode) error {
	if err := storage.wait(ctx, wire.Flush); err != nil {
		return err
	}
	return storage.Storage.Flush(ctx, handle, mode)
}

// From reconnect_netfault_test.go.
type reconnectClock struct {
	value        time.Time
	afterAdvance func()
	mu           sync.Mutex
}

// From reconnect_netfault_test.go.
func (clock *reconnectClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.value
}

// From reconnect_netfault_test.go.
func (clock *reconnectClock) advance(duration time.Duration) {
	clock.mu.Lock()
	clock.value = clock.value.Add(duration)
	afterAdvance := clock.afterAdvance
	clock.mu.Unlock()
	if afterAdvance != nil {
		afterAdvance()
	}
}

// From reconnect_netfault_test.go.
type reconnectFixture struct {
	server  *Server
	storage *reconnectStorage
	clock   *reconnectClock
	ctx     context.Context
	login   smbtest.LoginOptions
}

// From reconnect_netfault_test.go.
func newReconnectFixture(t *testing.T, cipher uint16) *reconnectFixture {
	t.Helper()
	clock := &reconnectClock{value: time.Now()}
	options := testOptions(t)
	options.Now = clock.now
	storage := &reconnectStorage{Storage: newFilesMetaStorage(t)}
	options.Storage = storage
	var err error
	options.State, err = state.New(clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Second)
		defer cleanupCancel()
		if err := server.Shutdown(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	return &reconnectFixture{
		server: server, storage: storage, clock: clock, ctx: ctx,
		login: smbtest.LoginOptions{Account: options.Account, Share: options.ShareName, ClientGUID: [16]byte{73}, Cipher: cipher, Signing: smb.SigningGMAC},
	}
}

// From reconnect_netfault_test.go.
type reconnectTransport struct {
	conn  net.Conn
	proxy *netfault.Proxy
	done  chan struct{}
}

// From reconnect_netfault_test.go.
// Each transport gets its own proxy, so cutting one client cannot cut the peer
// used to check detached sharing, ranges or a pending lease break.
func (fixture *reconnectFixture) transport(t *testing.T) *reconnectTransport {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(fixture.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
	})
	proxy, err := netfault.New(fixture.ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &reconnectTransport{proxy: proxy, done: make(chan struct{})}
	go func() {
		defer close(transport.done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if !errors.Is(acceptErr, net.ErrClosed) {
				t.Error(acceptErr)
			}
			return
		}
		if serveErr := fixture.server.ServeConn(fixture.ctx, conn); serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			t.Error(serveErr)
		}
	}()
	var dialer net.Dialer
	transport.conn, err = dialer.DialContext(fixture.ctx, "tcp", proxy.Address())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(fixture.ctx), 3*time.Second)
		defer cancel()
		awaitReconnectEvent(cleanupCtx, t, transport.done)
	})
	return transport
}

// From reconnect_netfault_test.go.
func awaitReconnectEvent(ctx context.Context, t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// From reconnect_netfault_test.go.
func (fixture *reconnectFixture) client(t *testing.T, guid [16]byte) (*smbtest.Client, smbtest.Session, *reconnectTransport) {
	t.Helper()
	transport := fixture.transport(t)
	client, err := smbtest.NewClient(transport.conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	options := fixture.login
	options.ClientGUID = guid
	session, err := client.Login(fixture.ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	return client, session, transport
}

// From reconnect_netfault_test.go.
func reconnectIO(t *testing.T, session *smbtest.Session, command wire.Command, id wire.FileID, data []byte, offset uint64) wire.Message {
	t.Helper()
	var body []byte
	var err error
	length := len(data)
	if length > 65536 {
		t.Fatal("reconnect test I/O exceeds one credit")
		return wire.Message{}
	}
	switch uint16(command) {
	case uint16(wire.Write):
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data, Offset: offset})
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: uint32(length), Offset: offset})
	case uint16(wire.Flush):
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{ID: id})
	default:
		t.Fatalf("unsupported reconnect I/O command %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(*session, session.NextMessageID, command, body, 1)
	session.NextMessageID++
	return message
}

// From replacement_inflight_test.go.
func checkReplacementInFlight(t *testing.T, command wire.Command) {
	t.Helper()
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	opened := make(chan state.Open, 1)
	server.handlers[command] = func(ctx context.Context, request RequestContext, _ wire.Message) (reply, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
			return reply{}, nil
		}
		<-release
		// A storage completion can publish a grant after cancellation. Cleanup
		// must catch it after draining, not only at the initial disconnect.
		object := smb.ObjectKey{Inode: 34}
		reservation, status := request.Opens.Reserve(state.OpenRequest{Object: object, Binding: request.Binding(), User: request.Session.User, Share: request.Tree.Share, GrantedAccess: 1})
		if status != smb.StatusSuccess {
			return reply{}, fmt.Errorf("late reservation: %#x", status)
		}
		open, status := request.Opens.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}})
		if status != smb.StatusSuccess {
			abortStatus := request.Opens.Abort(reservation)
			return reply{}, fmt.Errorf("late commit: %#x, abort: %#x", status, abortStatus)
		}
		opened <- open
		return reply{}, ctx.Err()
	}
	_, ctx, old := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	owner := onlyConnection(t, server)
	ordinary := insertSessionOpen(t, server, old, false, 32)
	durable := insertSessionOpen(t, server, old, true, 33)
	message := treeRequest(t, old, old.NextMessageID, wire.Read)
	message.Header.Command = command
	var operation *work
	if command == wire.Read {
		// Deliberately stop before waitLocal/sendPending. There is no timer or
		// scheduling race deciding whether this operation is in pending.
		operation = owner.startWork(ctx, message, nil, compoundState{})
	} else {
		operation = &work{done: make(chan struct{})}
		go func() { operation.result = owner.execute(ctx, message, compoundState{}); close(operation.done) }()
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	owner.pendingMu.Lock()
	pendingCount := len(owner.pending)
	owner.pendingMu.Unlock()
	if pendingCount != 0 {
		t.Fatal("test operation became pending")
	}
	client, freshCtx := pipeClient(t, server)
	type loginResult struct {
		err     error
		session smbtest.Session
	}
	finished := make(chan loginResult, 1)
	go func() {
		fresh, loginErr := client.Login(freshCtx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}, PreviousSessionID: old.SessionID})
		finished <- loginResult{session: fresh, err: loginErr}
	}()
	select {
	case <-canceled:
	case result := <-finished:
		t.Fatalf("replacement finished without canceling in-flight work: %v", result.err)
	case <-freshCtx.Done():
		t.Fatal(freshCtx.Err())
	}
	select {
	case <-finished:
		t.Fatal("replacement finished before in-flight work drained")
	default:
	}
	if storage.closed.Load() != 0 {
		t.Fatal("storage cleanup raced an active handler")
	}
	for _, open := range []state.Open{ordinary, durable} {
		if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
			t.Fatal("initial disconnect did not detach existing opens promptly")
		}
	}
	releaseOnce.Do(func() { close(release) })
	var result loginResult
	select {
	case result = <-finished:
	case <-freshCtx.Done():
		t.Fatal(freshCtx.Err())
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	<-operation.done
	if operation.result.status != smb.StatusCancelled {
		t.Fatalf("old work: %#x", operation.result.status)
	}
	late := <-opened
	if _, status := options.State.Find(late.ID, late.Binding); status == smb.StatusSuccess {
		t.Fatal("late open was orphaned on the removed session")
	}
	if storage.closed.Load() != 2 {
		t.Fatalf("closed %d handles, want existing and late ordinary opens", storage.closed.Load())
	}
	reservation, status := options.State.Reserve(state.OpenRequest{Object: late.Object, Binding: state.Binding{SessionID: result.session.SessionID, TreeID: result.session.TreeID}, GrantedAccess: 1, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatalf("orphan sharing prevents a fresh open: %#x", status)
	}
	if status := options.State.Abort(reservation); status != smb.StatusSuccess {
		t.Fatalf("fresh reservation cleanup: %#x", status)
	}
	if _, status := options.State.Reconnect(state.ReconnectRequest{ID: durable.ID, Binding: state.Binding{SessionID: result.session.SessionID, TreeID: result.session.TreeID}, User: durable.User, Share: durable.Share, ClientGUID: durable.ClientGUID, CreateGUID: durable.CreateGUID, LeaseKey: durable.LeaseKey}); status != smb.StatusSuccess {
		t.Fatalf("existing durable open was not preserved: %#x", status)
	}
	if result := owner.execute(ctx, message, compoundState{}); result.status != smb.StatusUserSessionDeleted {
		t.Fatal("removed identity accepted more work")
	}
}

// From reply_layout_test.go.
// Layouts come from MS-SMB2 2.2.1 (headers), 2.2.2 (ERROR), 2.2.4 and
// 2.2.4.1 (NEGOTIATE and contexts), and 2.2.29 (ECHO). Reply values follow
// 3.3.4.2, 3.3.4.4 and 3.3.5.3.1. Requests may use encoders and the login
// fixture uses the test client, but replies under test use ReceiveRaw.
const (
	layoutHeaderSize = 64
	layoutResponse   = 0x00000001
	layoutAsync      = 0x00000002
	layoutSigned     = 0x00000008
)

// From reply_layout_test.go.
type layoutField struct {
	name   string
	offset int
	width  int
	want   uint64
}

// From reply_layout_test.go.
func assertLayoutFields(t *testing.T, raw []byte, fields ...layoutField) {
	t.Helper()
	for _, field := range fields {
		if field.offset < 0 || field.width < 1 || field.offset > len(raw)-field.width {
			t.Fatalf("%s at offset %d needs %d bytes, reply has %d", field.name, field.offset, field.width, len(raw))
		}
		data := raw[field.offset : field.offset+field.width]
		var got uint64
		switch field.width {
		case 1:
			got = uint64(data[0])
		case 2:
			got = uint64(binary.LittleEndian.Uint16(data))
		case 4:
			got = uint64(binary.LittleEndian.Uint32(data))
		case 8:
			got = binary.LittleEndian.Uint64(data)
		default:
			t.Fatalf("unsupported field width %d", field.width)
		}
		if got != field.want {
			t.Errorf("%s at offset %d = %#x, want %#x", field.name, field.offset, got, field.want)
		}
	}
}

// From reply_layout_test.go.
type layoutHeader struct {
	status, flags, processID, treeID uint32
	messageID, sessionID, asyncID    uint64
	command, charge, credits         uint16
}

// From reply_layout_test.go.
func assertLayoutHeader(t *testing.T, raw []byte, want layoutHeader) {
	t.Helper()
	if len(raw) < layoutHeaderSize {
		t.Fatalf("SMB2 header has %d bytes, want at least 64", len(raw))
	}
	if !bytes.Equal(raw[:4], []byte{0xfe, 'S', 'M', 'B'}) {
		t.Errorf("protocol ID = %x, want fe534d42", raw[:4])
	}
	assertLayoutFields(t, raw,
		layoutField{"header structure size", 4, 2, 64},
		layoutField{"credit charge", 6, 2, uint64(want.charge)},
		layoutField{"status", 8, 4, uint64(want.status)},
		layoutField{"command", 12, 2, uint64(want.command)},
		layoutField{"credit response", 14, 2, uint64(want.credits)},
		layoutField{"flags", 16, 4, uint64(want.flags)},
		layoutField{"next command", 20, 4, 0},
		layoutField{"message ID", 24, 8, want.messageID},
		layoutField{"session ID", 40, 8, want.sessionID},
	)
	if want.flags&layoutAsync != 0 {
		assertLayoutFields(t, raw, layoutField{"async ID", 32, 8, want.asyncID})
	} else {
		assertLayoutFields(t, raw,
			// The reserved field was called ProcessId in older MS-SMB2 editions.
			layoutField{"reserved (process ID)", 32, 4, uint64(want.processID)},
			layoutField{"tree ID", 36, 4, uint64(want.treeID)},
		)
	}
	zeroSignature := bytes.Equal(raw[48:64], make([]byte, 16))
	if want.flags&layoutSigned != 0 {
		if zeroSignature {
			t.Error("signed reply has zero signature at offset 48")
		}
	} else if !zeroSignature {
		t.Errorf("unsigned signature at offset 48 = %x", raw[48:64])
	}
}

// From reply_layout_test.go.
func layoutExchange(ctx context.Context, t *testing.T, client *smbtest.Client, request wire.Message) []byte {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	return layoutReceive(ctx, t, client)
}

// From reply_layout_test.go.
func layoutReceive(ctx context.Context, t *testing.T, client *smbtest.Client) []byte {
	t.Helper()
	raw, err := client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// From reply_layout_test.go.
func assertLayoutError(t *testing.T, raw []byte) {
	t.Helper()
	if len(raw) != 73 {
		t.Fatalf("ERROR reply length = %d, want 73", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"ERROR structure size", 64, 2, 9},
		layoutField{"error context count", 66, 1, 0},
		layoutField{"ERROR reserved", 67, 1, 0},
		layoutField{"error byte count", 68, 4, 0},
		layoutField{"empty error data", 72, 1, 0},
	)
}

// From reply_layout_test.go.
func layoutNegotiateOptions(t *testing.T) Options {
	t.Helper()
	options := testOptions(t)
	options.Now = func() time.Time { return time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC) }
	options.ServerGUID = [16]byte{0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87, 0x98, 0xa9, 0xba, 0xcb, 0xdc, 0xed, 0xfe, 0x0f}
	return options
}

// From reply_layout_test.go.
func assertLayoutNegotiateFixed(t *testing.T, raw []byte, options Options, dialect uint16) {
	t.Helper()
	if len(raw) < 128 {
		t.Fatalf("NEGOTIATE reply length = %d, want at least 128", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"NEGOTIATE structure size", 64, 2, 65},
		layoutField{"security mode", 66, 2, 0x0003},
		layoutField{"dialect revision", 68, 2, uint64(dialect)},
		layoutField{"capabilities", 88, 4, 0x00000004},
		layoutField{"maximum transact size", 92, 4, 0x00100000},
		layoutField{"maximum read size", 96, 4, 0x00100000},
		layoutField{"maximum write size", 100, 4, 0x00100000},
		// FILETIME ticks from 1601-01-01 to the fixed clock above.
		layoutField{"system time", 104, 8, 134355744000000000},
		layoutField{"server start time", 112, 8, 0},
	)
	if !bytes.Equal(raw[72:88], options.ServerGUID[:]) {
		t.Errorf("server GUID at offset 72 = %x, want %x", raw[72:88], options.ServerGUID)
	}
}

// From response_test.go.
func responseBodyFixture(t *testing.T, command wire.Command) (wire.Message, []byte) {
	t.Helper()
	var request, response []byte
	var requestErr, responseErr error
	switch uint16(command) {
	case uint16(wire.SessionSetup):
		request, requestErr = wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{})
		response, responseErr = wire.EncodeSessionSetupResponse(wire.SessionSetupResponse{Token: []byte("challenge")})
	case uint16(wire.QueryInfo):
		request, requestErr = wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{OutputLength: 16})
		response, responseErr = wire.EncodeQueryInfoResponse(wire.QueryResponse{Data: []byte("partial")})
	case uint16(wire.IOCTL):
		request, requestErr = wire.EncodeIOCTLRequest(wire.IOCTLRequest{MaxOutput: 16})
		response, responseErr = wire.EncodeIOCTLResponse(wire.IOCTLResponse{Output: []byte("partial")})
	case uint16(wire.Read):
		request, requestErr = wire.EncodeReadRequest(wire.ReadRequest{Length: 16})
		response, responseErr = wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("partial")})
	case uint16(wire.Echo):
		request, requestErr = wire.EncodeEchoRequest(wire.EmptyRequest{})
		response, responseErr = wire.EncodeEchoResponse(wire.EmptyResponse{})
	default:
		t.Fatalf("no response fixture for command %d", command)
	}
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if responseErr != nil {
		t.Fatal(responseErr)
	}
	return wire.Message{Header: wire.Header{Command: command, MessageID: 1, CreditCharge: 1, Credit: 1}, Body: request}, response
}

// From scavenger_blocked_test.go.
type delayedExpiryClose struct {
	*expiryNotifications
	entered chan struct{}
	release chan struct{}
}

// From scavenger_blocked_test.go.
func (storage *delayedExpiryClose) Close(ctx context.Context, handle smb.Handle) error {
	if handle.Key().Inode == 2 {
		close(storage.entered)
		<-storage.release
	}
	return storage.expiryNotifications.Close(ctx, handle)
}

// From scavenger_blocked_test.go.
func checkExpiryWhileCleanupBlocked(t *testing.T, block string) {
	t.Helper()
	storage := &expiryNotifications{cleanupStorage: &cleanupStorage{}, closedHandles: make(chan smb.Handle, 3)}
	delayed := &delayedExpiryClose{expiryNotifications: storage, entered: make(chan struct{}), release: make(chan struct{})}
	var backend smb.Storage = storage
	if block == "close" {
		backend = delayed
	}
	server, clock := expiryServer(t, backend)
	ticks := fakeExpiryTicks(t, server)
	request := expiryRequest(2, 1)
	grant := expiryGrant(request)
	grant.DeleteOnClose = true
	grant.DeleteName = smb.Name{Parent: 1, Base: "old"}
	first := commitExpiryOpen(t, server, request, grant)
	unblock := blockExpiryCleanup(t, server, first, block, delayed)
	detachExpiryOpen(t, server, first)
	clock.advance(first.DurableTimeout)
	sendExpiryTick(t, ticks)
	if block == "parent" {
		waitExpiredHandle(t, storage)
	}
	if block == "close" {
		select {
		case <-delayed.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("expiry did not enter blocked Close")
		}
	}
	// This lease deadline starts only after the earlier expiry pass began.
	holderRequest := expiryRequest(3, 2)
	holderGrant := expiryGrant(holderRequest)
	holderGrant.Lease.State |= smb.LeaseWrite
	holder := commitExpiryOpen(t, server, holderRequest, holderGrant)
	breaks, actions := server.options.State.BreakLeases(holder.Object, state.GUID{9}, state.GUID{}, smb.LeaseRead|smb.LeaseHandle)
	if len(breaks) != 1 || !breaks[0].AckRequired || len(actions) != 0 {
		t.Fatalf("later break: %+v, cleanup: %+v", breaks, actions)
	}
	laterRequest := expiryRequest(4, 3)
	laterGrant := expiryGrant(laterRequest)
	laterGrant.DurableTimeout = state.LeaseBreakTimeout
	later := commitExpiryOpen(t, server, laterRequest, laterGrant)
	detachExpiryOpen(t, server, later)
	clock.advance(state.LeaseBreakTimeout)
	sendExpiryTick(t, ticks)
	// Taking the next tick proves the previous table-only pass has finished.
	sendExpiryTick(t, ticks)
	found, status := server.options.State.Find(holder.ID, holder.Binding)
	if status != smb.StatusSuccess || found.Durable || found.DurableTimeout != 0 {
		t.Fatalf("blocked cleanup delayed break revocation: %+v, status %#x", found, status)
	}
	if _, _, status := server.options.State.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, smb.LeaseRead); status != smb.StatusUnsuccessful {
		t.Fatalf("timed-out break still pending: %#x", status)
	}
	waitExpiredHandle(t, storage)
	if storage.removed.Load() != 0 {
		t.Fatal("blocked expiry deletion ran before cleanup was released")
	}
	assertShutdownDrainsBlockedExpiry(t, server)
	unblock()
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 3 || storage.removed.Load() != 1 {
		t.Fatalf("shutdown did not drain all cleanup: close %d, remove %d", storage.closed.Load(), storage.removed.Load())
	}
}

// From scavenger_blocked_test.go.
func blockExpiryCleanup(t *testing.T, server *Server, open state.Open, block string, storage *delayedExpiryClose) func() {
	t.Helper()
	var release func()
	switch block {
	case "parent":
		unlock, err := lockParent(t.Context(), RequestContext{server: server}, 1)
		if err != nil {
			t.Fatal(err)
		}
		release = unlock
	case "close":
		release = func() { close(storage.release) }
	case "open use":
		_, releaseOpen, status := useOpen(openRequestContext(server, open), wire.FileID(open.ID))
		if status != smb.StatusSuccess {
			t.Fatal(status)
		}
		release = releaseOpen
	default:
		t.Fatalf("unknown cleanup blocker %q", block)
	}
	var once sync.Once
	unblock := func() { once.Do(release) }
	t.Cleanup(unblock)
	return unblock
}

// From scavenger_blocked_test.go.
func assertShutdownDrainsBlockedExpiry(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled shutdown: %v", err)
	}
	select {
	case <-server.scavengerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked cleanup prevented the expiry timer from stopping")
	}
	select {
	case <-server.shutdownDone:
		t.Fatal("shutdown finished while an expiry cleanup was blocked")
	default:
	}
}

// From scavenger_loop_test.go.
type expiryNotifications struct {
	*cleanupStorage
	closedHandles chan smb.Handle
}

// From scavenger_loop_test.go.
func (storage *expiryNotifications) Close(ctx context.Context, handle smb.Handle) error {
	err := storage.cleanupStorage.Close(ctx, handle)
	storage.closedHandles <- handle
	return err
}

// From scavenger_loop_test.go.
func fakeExpiryTicks(t *testing.T, server *Server) chan time.Time {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	server.scavengerStop = make(chan struct{})
	server.scavengerDone = make(chan struct{})
	ticks := make(chan time.Time)
	go server.runScavenger(context.WithoutCancel(t.Context()), ticks)
	return ticks
}

// From scavenger_loop_test.go.
func sendExpiryTick(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case <-time.After(3 * time.Second):
		t.Fatal("scavenger did not take a tick")
	}
}

// From scavenger_loop_test.go.
func waitExpiredHandle(t *testing.T, storage *expiryNotifications) {
	t.Helper()
	select {
	case <-storage.closedHandles:
	case <-time.After(3 * time.Second):
		t.Fatal("scavenger did not close the handle")
	}
}

// From scavenger_test.go.
type expiryClock struct {
	now time.Time
	mu  sync.Mutex
}

// From scavenger_test.go.
func (clock *expiryClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

// From scavenger_test.go.
func (clock *expiryClock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// From scavenger_test.go.
func expiryServer(t *testing.T, storage smb.Storage) (*Server, *expiryClock) {
	t.Helper()
	clock := &expiryClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	options := testOptions(t)
	options.Now = clock.Now
	var err error
	options.State, err = state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return server, clock
}

// From scavenger_test.go.
func expiryRequest(inode smb.Inode, session uint64) state.OpenRequest {
	return state.OpenRequest{
		Object: smb.ObjectKey{Inode: inode}, Binding: state.Binding{SessionID: session, TreeID: 1},
		ClientGUID: state.GUID{1}, CreateGUID: state.GUID{byte(inode & 0xff), byte(session & 0xff)},
		GrantedAccess: 0x10001, Sharing: state.ShareMode(state.RightRead | state.RightDelete),
	}
}

// From scavenger_test.go.
func expiryGrant(request state.OpenRequest) state.Grant {
	return state.Grant{
		Handle: cleanupHandle{object: request.Object}, DurableTimeout: 2 * time.Minute,
		Lease: state.Lease{ClientGUID: request.ClientGUID, Key: state.GUID{byte(request.Object.Inode & 0xff)}, State: smb.LeaseRead | smb.LeaseHandle},
	}
}

// From scavenger_test.go.
func commitExpiryOpen(t *testing.T, server *Server, request state.OpenRequest, grant state.Grant) state.Open {
	t.Helper()
	token, status := server.options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(token, grant)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

// From scavenger_test.go.
func detachExpiryOpen(t *testing.T, server *Server, open state.Open) {
	t.Helper()
	if actions := server.options.State.Disconnect(open.Binding.SessionID); len(actions) != 0 {
		t.Fatalf("durable open closed on disconnect: %+v", actions)
	}
}

// From sender_test.go.
type controlledWrite struct {
	release chan error
}

// From sender_test.go.
type controlledConn struct {
	net.Conn
	writes chan controlledWrite
	count  atomic.Int32
}

// From sender_test.go.
func (conn *controlledConn) Write(data []byte) (int, error) {
	conn.count.Add(1)
	call := controlledWrite{release: make(chan error, 1)}
	conn.writes <- call
	err := <-call.release
	if err == nil {
		return conn.Conn.Write(data)
	}
	n, writeErr := conn.Conn.Write(data[:5])
	return n, errors.Join(err, writeErr)
}

// From sender_test.go.
func senderPipe(t *testing.T) (*sender, *controlledConn, *smbtest.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	local, remote := net.Pipe()
	conn := &controlledConn{Conn: local, writes: make(chan controlledWrite, 3)}
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	sender := newSender(conn)
	go sender.run(ctx, conn.Close)
	t.Cleanup(func() {
		cancel()
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-sender.done:
		case <-time.After(3 * time.Second):
			t.Error("sender did not stop")
		}
	})
	return sender, conn, client, ctx
}

// From sender_test.go.
func waitWrite(ctx context.Context, t *testing.T, conn *controlledConn) controlledWrite {
	t.Helper()
	select {
	case call := <-conn.writes:
		return call
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return controlledWrite{}
	}
}

// From sender_test.go.
func noCompletion(t *testing.T, completion <-chan error) {
	t.Helper()
	select {
	case err := <-completion:
		t.Fatalf("premature sender completion: %v", err)
	default:
	}
}

// From server_test.go.
// Connection tests do not perform filesystem operations.
type unusedStorage struct{ smb.Storage }

// From server_test.go.
func testOptions(t *testing.T) Options {
	t.Helper()
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Storage: unusedStorage{}, State: table, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		Account: auth.Account{User: "backup", Password: "password"}, ShareName: "backup", ServerName: "s3-smb", ServerGUID: [16]byte{1},
	}
}

// From server_test.go.
func pipeClient(t *testing.T, server *Server) (*smbtest.Client, context.Context) {
	t.Helper()
	return configuredPipeClient(t, server, false)
}

// From server_test.go.
// corePipeClient supplies identity without authentication or protection. These
// tests isolate framing and async plumbing; session tests use real Login.
func corePipeClient(t *testing.T, server *Server) (*smbtest.Client, context.Context) {
	t.Helper()
	return configuredPipeClient(t, server, true)
}

// From server_test.go.
func configuredPipeClient(t *testing.T, server *Server, coreIdentity bool) (*smbtest.Client, context.Context) {
	t.Helper()
	return boundedPipeClient(t, server, coreIdentity, 3*time.Second)
}

// From server_test.go.
func boundedPipeClient(t *testing.T, server *Server, coreIdentity bool, bound time.Duration) (*smbtest.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	connCtx, connCancel := context.WithCancel(ctx)
	connection, err := server.addConnection(connCtx, connCancel, local)
	if err != nil {
		t.Fatal(err)
	}
	if coreIdentity {
		connection.sessions[77] = &sessionEntry{identity: Session{SessionID: 77}, active: true, trees: map[uint32]Tree{12: {TreeID: 12, Share: "backup"}}}
	}
	done := make(chan error, 1)
	go func() { done <- server.runConnection(connCtx, connection) }()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return client, ctx
}

// From server_test.go.
func exchange(ctx context.Context, t *testing.T, client *smbtest.Client, messages ...wire.Message) []wire.Message {
	t.Helper()
	if err := client.Send(ctx, messages); err != nil {
		t.Fatal(err)
	}
	reply, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return reply.Messages
}

// From server_test.go.
func echo(t testing.TB, id uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.Echo, MessageID: id, CreditCharge: 1, Credit: 1}, Body: body}
}

// From session_cleanup_test.go.
func insertSessionOpen(t *testing.T, server *Server, session smbtest.Session, durable bool, inode smb.Inode) state.Open {
	t.Helper()
	object := smb.ObjectKey{Inode: inode}
	client := state.GUID{2}
	request := state.OpenRequest{Object: object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: client, GrantedAccess: 1, Sharing: 7}
	grant := state.Grant{Handle: cleanupHandle{object: object}}
	if durable {
		request.CreateGUID = state.GUID{byte(inode & 0xff)}
		grant.DurableTimeout = time.Minute
		grant.Lease = state.Lease{ClientGUID: client, Key: state.GUID{byte(inode & 0xff)}, State: smb.LeaseRead | smb.LeaseHandle}
	}
	reservation, status := server.options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, grant)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

// From session_cleanup_test.go.
func treeRequest(t *testing.T, session smbtest.Session, id uint64, command wire.Command) wire.Message {
	t.Helper()
	var body []byte
	var err error
	switch uint16(command) {
	case uint16(wire.TreeDisconnect):
		body, err = wire.EncodeTreeDisconnectRequest(wire.EmptyRequest{})
	case uint16(wire.Logoff):
		body, err = wire.EncodeLogoffRequest(wire.EmptyRequest{})
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{Length: 1})
	case uint16(wire.TreeConnect):
		body, err = wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\server\\backup"})
	default:
		t.Fatalf("no body for command %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: command, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}, Body: body}
}

// From session_cleanup_test.go.
func checkSessionCleanup(t *testing.T, command wire.Command, cipher uint16) {
	t.Helper()
	options := testOptions(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	_, _, other := loginClient(t, server, cipher, smb.SigningGMAC)
	owned := []state.Open{insertSessionOpen(t, server, session, false, 2), insertSessionOpen(t, server, session, true, 3)}
	unrelated := insertSessionOpen(t, server, other, false, 4)
	response := exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, command))[0]
	if response.Header.Status != smb.StatusSuccess || storage.closed.Load() != 2 {
		t.Fatalf("cleanup: %+v, closed %d", response.Header, storage.closed.Load())
	}
	for _, open := range owned {
		if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
			t.Fatal("owned open survived cleanup")
		}
	}
	if _, status := options.State.Find(unrelated.ID, unrelated.Binding); status != smb.StatusSuccess {
		t.Fatal("another session's open was closed")
	}
	next := session.NextMessageID + 1
	if command == wire.Logoff {
		payload, err := wire.Join([]wire.Message{treeRequest(t, session, next, wire.Read)})
		if err != nil {
			t.Fatal(err)
		}
		sendPayload(ctx, t, client, payload)
		payload, err = client.ReceiveRaw(ctx)
		if err != nil {
			t.Fatal(err)
		}
		members, err := wire.Split(payload)
		if err != nil {
			t.Fatal(err)
		}
		if members[0].Header.Status != smb.StatusUserSessionDeleted {
			t.Fatal("logged-off identity remains valid")
		}
	} else {
		response = exchange(ctx, t, client, treeRequest(t, session, next, wire.Read))[0]
		if response.Header.Status != smb.StatusNetworkNameDeleted {
			t.Fatalf("deleted tree: %+v", response.Header)
		}
	}
	if command == wire.TreeDisconnect {
		response = exchange(ctx, t, client, sessionEcho(t, session, next+1))[0]
		if response.Header.Status != smb.StatusSuccess {
			t.Fatal("disconnect removed the session")
		}
	}
}

// From session_compound_test.go.
func checkRelatedCleanup(t *testing.T, command wire.Command) {
	t.Helper()
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		select {
		case <-release:
			body, encodeErr := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			return reply{body: body}, encodeErr
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	open := insertSessionOpen(t, server, session, false, 12)
	read := treeRequest(t, session, session.NextMessageID, wire.Read)
	cleanup := treeRequest(t, session, session.NextMessageID+1, command)
	cleanup.Header.Flags = wire.FlagRelated
	cleanup.Header.SessionID, cleanup.Header.TreeID = ^uint64(0), ^uint32(0)
	if err := client.Send(ctx, []wire.Message{read, cleanup}); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		if header.Status != smb.StatusPending || header.MessageID != session.NextMessageID+uint64(index) {
			t.Fatal(header)
		}
	}
	close(release)
	seen := make(map[wire.Command]bool)
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		if header.Status != smb.StatusSuccess || header.Credit != 0 || seen[header.Command] {
			t.Fatal(header)
		}
		seen[header.Command] = true
	}
	if !seen[wire.Read] || !seen[command] {
		t.Fatal("missing final response")
	}
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("related cleanup did not close the open")
	}
}

// From session_lifecycle_test.go.
func onlyConnection(t *testing.T, server *Server) *connection {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.connections) != 1 {
		t.Fatal("expected one connection")
	}
	for owner := range server.connections {
		return owner
	}
	t.Fatal("connection was not registered")
	return nil
}

// From session_rejection_test.go.
func kerberosOffer(t *testing.T) []byte {
	t.Helper()
	sequence, err := asn1.Marshal(struct {
		Mechs []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
	}{Mechs: []asn1.ObjectIdentifier{{1, 2, 840, 113554, 1, 2, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	choice, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sequence})
	if err != nil {
		t.Fatal(err)
	}
	oid, err := asn1.Marshal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2})
	if err != nil {
		t.Fatal(err)
	}
	token, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassApplication, Tag: 0, IsCompound: true, Bytes: append(oid, choice...)})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// From session_reply_layout_test.go.
// This exchange deliberately bypasses Client.Login, which decodes the replies
// under test. Tokens come from offset 72, not from DecodeSessionSetupResponse.
func sessionLayoutLogin(t *testing.T, server *Server) (*smbtest.Client, context.Context, uint64, *crypt.Protector) {
	t.Helper()
	client, ctx := pipeClient(t, server)
	preauth := crypt.NewPreauth()
	request := negotiateMessage(t, 16)
	payload, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	preauth.Update(payload)
	negotiation := exchange(ctx, t, client, request)[0]
	preauth.Update(negotiation.Raw)
	negotiated, err := wire.DecodeNegotiateResponse(negotiation)
	if err != nil {
		t.Fatal(err)
	}
	initiator, err := auth.NewInitiator(server.options.Account, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initiator.Start(negotiated.Token)
	if err != nil {
		t.Fatal(err)
	}
	var sessionID uint64
	var protector *crypt.Protector
	for messageID := uint64(1); messageID <= 2; messageID++ {
		body, encodeErr := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, SecurityMode: 3})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		request = wire.Message{Header: wire.Header{
			Command: wire.SessionSetup, MessageID: messageID, SessionID: sessionID,
			ProcessID: 0x12345678, CreditCharge: 1, Credit: 16,
		}, Body: body}
		payload, err = wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		preauth.Update(payload)
		raw := layoutExchange(ctx, t, client, request)
		if len(raw) <= 72 {
			t.Fatalf("SESSION_SETUP reply length = %d, want a token after offset 72", len(raw))
		}
		want := layoutHeader{
			status: 0xc0000016, flags: layoutResponse, command: 0x0001, charge: 1, credits: 16,
			messageID: messageID, processID: 0x12345678, sessionID: sessionID,
		}
		var sessionFlags uint16
		if messageID == 1 {
			sessionID = binary.LittleEndian.Uint64(raw[40:48])
			if sessionID == 0 || sessionID == ^uint64(0) {
				t.Fatalf("invalid allocated session ID at offset 40: %#x", sessionID)
			}
			want.sessionID = sessionID
			preauth.Update(raw)
		} else {
			want.status, want.flags = 0, layoutResponse|layoutSigned
			if server.options.Encryption == RequireEncryption {
				sessionFlags = 0x0004
			}
			protector, err = crypt.NewProtector(crypt.Options{
				SessionKey: result.SessionKey, Preauth: preauth.Sum(), SessionID: sessionID,
				Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, Role: crypt.RoleClient,
			})
			if err != nil {
				t.Fatal(err)
			}
			if verifyErr := protector.Verify(raw); verifyErr != nil {
				t.Fatalf("final SESSION_SETUP signature: %v", verifyErr)
			}
			// RFC 4178 NegTokenResp, accept-completed, with no response token.
			accepted := []byte{0xa1, 0x07, 0x30, 0x05, 0xa0, 0x03, 0x0a, 0x01, 0x00}
			if !bytes.Equal(raw[72:], accepted) {
				t.Fatalf("final security buffer at offset 72 = %x, want %x", raw[72:], accepted)
			}
		}
		assertLayoutHeader(t, raw, want)
		assertLayoutFields(t, raw,
			layoutField{"SESSION_SETUP structure size", 64, 2, 9},
			layoutField{"session flags", 66, 2, uint64(sessionFlags)},
			layoutField{"security buffer offset", 68, 2, 72},
			layoutField{"security buffer length", 70, 2, uint64(len(raw)) - 72},
		)
		result, err = initiator.Step(raw[72:])
		if err != nil {
			t.Fatalf("SESSION_SETUP security buffer: %v", err)
		}
	}
	if !result.Done {
		t.Fatal("authentication did not complete")
	}
	return client, ctx, sessionID, protector
}

// From session_reply_layout_test.go.
func sessionLayoutProtectedExchange(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector, encrypted bool, request wire.Message) []byte {
	t.Helper()
	var payload []byte
	if encrypted {
		plain, err := wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		payload, err = protector.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		payload = signMessages(t, protector, request)
	}
	sendPayload(ctx, t, client, payload)
	raw := layoutReceive(ctx, t, client)
	if encrypted {
		plain, err := protector.Open(raw)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	if err := protector.Verify(raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// From session_reply_layout_test.go.
func checkTreeAndCleanupLayouts(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector, sessionID uint64, encrypted bool) {
	t.Helper()
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: `\\server\backup`})
	if err != nil {
		t.Fatal(err)
	}
	request := wire.Message{Header: wire.Header{
		Command: wire.TreeConnect, MessageID: 3, SessionID: sessionID,
		ProcessID: 0x76543210, CreditCharge: 1, Credit: 7,
	}, Body: body}
	raw := sessionLayoutProtectedExchange(ctx, t, client, protector, encrypted, request)
	if len(raw) != 80 {
		t.Fatalf("TREE_CONNECT reply length = %d, want 80", len(raw))
	}
	treeID := binary.LittleEndian.Uint32(raw[36:40])
	if treeID == 0 || treeID == ^uint32(0) {
		t.Fatalf("invalid allocated tree ID at offset 36: %#x", treeID)
	}
	flags := uint32(layoutResponse)
	if !encrypted {
		flags |= layoutSigned
	}
	want := layoutHeader{
		flags: flags, command: 0x0003, charge: 1, credits: 7,
		messageID: 3, processID: 0x76543210, sessionID: sessionID, treeID: treeID,
	}
	assertLayoutHeader(t, raw, want)
	assertLayoutFields(t, raw,
		layoutField{"TREE_CONNECT structure size", 64, 2, 16},
		layoutField{"share type (disk)", 66, 1, 0x01},
		layoutField{"TREE_CONNECT reserved", 67, 1, 0},
		layoutField{"share flags", 68, 4, 0},
		layoutField{"share capabilities", 72, 4, 0},
		layoutField{"maximal access", 76, 4, 0x001f01ff},
	)

	body, err = wire.EncodeTreeDisconnectRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Command, request.Header.MessageID, request.Header.TreeID = wire.TreeDisconnect, 4, treeID
	request.Body = body
	raw = sessionLayoutProtectedExchange(ctx, t, client, protector, encrypted, request)
	want.command, want.messageID = 0x0004, 4
	assertLayoutHeader(t, raw, want)
	assertSessionCleanupLayout(t, raw, "TREE_DISCONNECT")

	body, err = wire.EncodeLogoffRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Command, request.Header.MessageID, request.Header.TreeID = wire.Logoff, 5, 0
	request.Body = body
	raw = sessionLayoutProtectedExchange(ctx, t, client, protector, encrypted, request)
	want.command, want.messageID, want.treeID = 0x0002, 5, 0
	assertLayoutHeader(t, raw, want)
	assertSessionCleanupLayout(t, raw, "LOGOFF")
}

// From session_reply_layout_test.go.
func assertSessionCleanupLayout(t *testing.T, raw []byte, command string) {
	t.Helper()
	if len(raw) != 68 {
		t.Fatalf("%s reply length = %d, want 68", command, len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{command + " structure size", 64, 2, 4},
		layoutField{command + " reserved", 66, 2, 0},
	)
}

// From session_test.go.
func loginClient(t *testing.T, server *Server, cipher, signing uint16) (*smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	client, ctx := pipeClient(t, server)
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: cipher, Signing: signing, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return client, ctx, session
}

// From session_test.go.
func sessionEcho(t *testing.T, session smbtest.Session, id uint64) wire.Message {
	t.Helper()
	message := echo(t, id)
	message.Header.SessionID = session.SessionID
	return message
}

// From set_info_allocation_race_test.go.
// Pause one mutation before entering the real adapter's inode coordinator.
type pausedAllocationStorage struct {
	smb.Storage
	entered chan struct{}
	resume  chan struct{}
	armed   atomic.Bool
	once    sync.Once
}

// From set_info_allocation_race_test.go.
func (s *pausedAllocationStorage) SetAttr(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Storage.SetAttr(ctx, object, change)
}

// From set_info_allocation_race_test.go.
func (s *pausedAllocationStorage) unblock() {
	s.once.Do(func() { close(s.resume) })
}

// From set_info_allocation_race_test.go.
func checkAllocationAfterEOFShrink(t *testing.T, path string) {
	t.Helper()
	storage := &pausedAllocationStorage{
		Storage: newFilesMetaStorage(t), entered: make(chan struct{}), resume: make(chan struct{}),
	}
	allocator := newReadWriteClient(t, storage)
	client, ctx, session := newFilesMetaClient(t, allocator.server)
	other := &readWriteClient{client: client, server: allocator.server, ctx: ctx, session: session, next: session.NextMessageID}
	// Release before either connection's cleanup, even on assertion failure.
	t.Cleanup(storage.unblock)
	selected := createdFile(t, allocator.create(t, createRequest("data", fileCreateDisposition)))
	if path != "data" {
		selected = createdFile(t, allocator.create(t, createRequest(path, fileCreateDisposition)))
	}
	competing := createdFile(t, other.create(t, createRequest(path, fileOpen)))
	requireIOStatus(t, allocator.write(t, wire.WriteRequest{ID: selected.ID, Data: bytes.Repeat([]byte("a"), 9000)}, 1), smb.StatusSuccess)
	request := allocationRaceSizeRequest(t, allocator, selected.ID, wire.ClassFileAllocation, 4097)
	storage.armed.Store(true)
	if err := allocator.client.Send(allocator.ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-storage.entered:
	case <-allocator.ctx.Done():
		t.Fatal(allocator.ctx.Err())
	}
	eof := allocationRaceSizeRequest(t, other, competing.ID, wire.ClassFileEndOfFile, 7)
	requireIOStatus(t, ioRoundTrip(other.ctx, t, other.client, eof), smb.StatusSuccess)
	storage.unblock()
	finishPausedAllocation(t, allocator, request)
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	attr, err := storage.GetAttr(t.Context(), resolved.Object)
	if err != nil || attr.Size != 7 {
		t.Fatalf("allocation re-extended EOF: size %d, want 7; error %v", attr.Size, err)
	}
	data, err := wire.DecodeReadResponse(other.read(t, wire.ReadRequest{ID: competing.ID, Length: 9000}, 1))
	if err != nil || string(data.Data) != "aaaaaaa" {
		t.Fatalf("retained bytes: %q, %v", data.Data, err)
	}
	if path != "data" {
		baseAttr, lookupErr := storage.Lookup(t.Context(), "data")
		if lookupErr != nil || baseAttr.Attr.Size != 0 {
			t.Fatalf("stream changed base: %+v, %v", baseAttr, lookupErr)
		}
	}
}

// From set_info_allocation_race_test.go.
func allocationRaceSizeRequest(t *testing.T, client *readWriteClient, id wire.FileID, class wire.FileInfoClass, size uint64) wire.Message {
	t.Helper()
	var input []byte
	var err error
	if class == wire.ClassFileAllocation {
		input, err = wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: size})
	} else {
		input, err = wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: size})
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.SetInfo, body, 1)
	client.next++
	return message
}

// From set_info_allocation_race_test.go.
func finishPausedAllocation(t *testing.T, client *readWriteClient, request wire.Message) {
	t.Helper()
	response, err := client.client.Receive(client.ctx)
	if err != nil || len(response.Messages) != 1 {
		t.Fatalf("allocation reply: %+v, %v", response.Messages, err)
	}
	if response.Messages[0].Header.Status == smb.StatusPending {
		pending := response.Messages[0]
		response, err = client.client.Receive(client.ctx)
		if err != nil || len(response.Messages) != 1 {
			t.Fatalf("allocation final: %+v, %v", response.Messages, err)
		}
		assertAsyncFinal(t, response.Messages[0], pending, smb.StatusSuccess)
	}
	final := response.Messages[0]
	if final.Header.MessageID != request.Header.MessageID || final.Header.Command != wire.SetInfo || final.Header.SessionID != client.session.SessionID {
		t.Fatalf("allocation identity: %+v", final.Header)
	}
	requireIOStatus(t, final, smb.StatusSuccess)
}

// From set_info_async_core_test.go.
func setInfoAsyncMessage(t *testing.T, id uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileEndOfFile), Input: make([]byte, 8)})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.SetInfo, MessageID: id, SessionID: 77, TreeID: 12, CreditCharge: 1, Credit: 16}, Body: body}
}

// From set_info_async_core_test.go.
func setInfoTestReply(t *testing.T) reply {
	t.Helper()
	body, err := wire.EncodeSetInfoResponse(wire.EmptyResponse{})
	if err != nil {
		t.Fatal(err)
	}
	return reply{body: body}
}

// From set_info_async_core_test.go.
func receiveSetInfoReply(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply, got %+v", response.Messages)
	}
	return response.Messages[0]
}

// From set_info_async_core_test.go.
func assertSetInfoAsync(t *testing.T, request, response wire.Message, status smb.Status, asyncID uint64, credits uint16) {
	t.Helper()
	header := response.Header
	if header.Command != wire.SetInfo || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.Status != status || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || header.AsyncID != asyncID || header.TreeID != 0 || header.Credit != credits || header.CreditCharge != request.Header.CreditCharge {
		t.Fatalf("SET_INFO async identity/credits: %+v", header)
	}
}

// From set_info_async_core_test.go.
func assertSetInfoEcho(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64) {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{sessionEcho(t, session, id)}); err != nil {
		t.Fatal(err)
	}
	response := receiveSetInfoReply(ctx, t, client)
	if response.Header.Command != wire.Echo || response.Header.MessageID != id || response.Header.SessionID != session.SessionID || response.Header.Status != smb.StatusSuccess || response.Header.Flags&wire.FlagAsync != 0 {
		t.Fatalf("extra reply or failed ECHO: %+v", response.Header)
	}
	if _, err := wire.DecodeEchoResponse(response); err != nil {
		t.Fatal(err)
	}
}

// From set_info_async_test.go.
// Delay only the metadata mutation. All file operations use the real adapter.
type blockedSetInfoStorage struct {
	smb.Storage
	started chan struct{}
	resume  chan struct{}
	failure error
	calls   atomic.Int32
	once    sync.Once
}

// From set_info_async_test.go.
func (s *blockedSetInfoStorage) SetAttr(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error {
	s.calls.Add(1)
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.resume:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.failure != nil {
		return s.failure
	}
	return s.Storage.SetAttr(ctx, object, change)
}

// From set_info_async_test.go.
func (s *blockedSetInfoStorage) unblock() {
	s.once.Do(func() { close(s.resume) })
}

// From set_info_async_test.go.
func newBlockedSetInfoFixture(t *testing.T, failure error) (*setInfoFixture, *blockedSetInfoStorage) {
	t.Helper()
	storage := &blockedSetInfoStorage{
		Storage: newFilesMetaStorage(t), started: make(chan struct{}, 1), resume: make(chan struct{}), failure: failure,
	}
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := newFilesMetaClient(t, server)
	open := insertFilesMetaOpen(t, server, session, "data", 0x102)
	// Unblock before server cleanup, including when an assertion fails.
	t.Cleanup(storage.unblock)
	return &setInfoFixture{storage: storage, client: client, ctx: ctx, session: session, open: open, nextID: session.NextMessageID}, storage
}

// From set_info_async_test.go.
func setInfoAsyncRequest(t *testing.T, f *setInfoFixture, class wire.FileInfoClass) wire.Message {
	t.Helper()
	var input []byte
	var err error
	switch uint8(class) {
	case uint8(wire.ClassFileBasic):
		value := filetime(t, time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
		input, err = wire.EncodeFileBasicInformation(wire.FileBasicInformation{Accessed: value, Modified: value, Changed: value})
	case uint8(wire.ClassFileEndOfFile):
		input, err = wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: 5000})
	case uint8(wire.ClassFileAllocation):
		input, err = wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: 4096})
	default:
		t.Fatalf("unexpected metadata class: %d", class)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{
		ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{
		Command: wire.SetInfo, MessageID: f.nextID, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: 1, Credit: 16,
	}, Body: body}
}

// From set_info_async_test.go.
func receiveSetInfoAsync(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply, got %+v", response.Messages)
	}
	return response.Messages[0]
}

// From set_info_async_test.go.
func assertSetInfoAsyncEcho(t *testing.T, f *setInfoFixture, id uint64) {
	t.Helper()
	responses := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, id))
	if len(responses) != 1 {
		t.Fatalf("ECHO replies: %+v", responses)
	}
	header := responses[0].Header
	if header.Command != wire.Echo || header.MessageID != id || header.SessionID != f.session.SessionID || header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 {
		t.Fatalf("extra reply or failed ECHO: %+v", header)
	}
}

// From set_info_async_test.go.
func assertSetInfoQuiet(t *testing.T, f *setInfoFixture) {
	t.Helper()
	// Receive cancellation closes the client, so this is the last wire assertion.
	ctx, cancel := context.WithTimeout(f.ctx, 25*time.Millisecond)
	defer cancel()
	response, err := f.client.Receive(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("extra reply after completion: %+v, error %v", response.Messages, err)
	}
}

// From set_info_async_test.go.
func checkSetInfoBufferedPending(t *testing.T, class wire.FileInfoClass, failure error) {
	t.Helper()
	f, storage := newBlockedSetInfoFixture(t, failure)
	data := bytes.Repeat([]byte("123456789"), 1000)
	f.write(t, string(data))
	before := f.attr(t)
	request := setInfoAsyncRequest(t, f, class)
	if err := f.client.Send(f.ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-storage.started:
	case <-f.ctx.Done():
		t.Fatal("SET_INFO did not reach storage")
	}
	pending := receiveSetInfoAsync(f.ctx, t, f.client)
	header := pending.Header
	if header.Command != wire.SetInfo || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.TreeID != 0 || header.CreditCharge != 1 || header.Credit != 16 {
		t.Fatalf("SET_INFO pending identity/credits: %+v", header)
	}
	assertSetInfoAsyncEcho(t, f, request.Header.MessageID+1)
	if after := f.attr(t); after != before {
		t.Fatalf("blocked SET_INFO changed metadata: before %+v, after %+v", before, after)
	}
	storage.unblock()
	final := receiveSetInfoAsync(f.ctx, t, f.client)
	header = final.Header
	want := smb.StatusSuccess
	if failure != nil {
		want = smb.StatusIODeviceError
	}
	if header.Command != wire.SetInfo || header.Status != want || header.Flags&wire.FlagAsync == 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.AsyncID != pending.Header.AsyncID || header.TreeID != 0 || header.CreditCharge != 1 || header.Credit != 0 {
		t.Fatalf("SET_INFO final identity/credits: %+v, want status %#x", header, want)
	}
	if storage.calls.Load() != 1 {
		t.Fatalf("SetAttr calls = %d, want 1", storage.calls.Load())
	}
	if failure == nil {
		if _, err := wire.DecodeSetInfoResponse(final); err != nil {
			t.Fatal(err)
		}
		assertSetInfoBufferedResult(t, f, class, data)
	} else if after := f.attr(t); after != before {
		t.Fatalf("failed SET_INFO changed metadata: before %+v, after %+v", before, after)
	}
	assertSetInfoAsyncEcho(t, f, request.Header.MessageID+2)
	assertSetInfoQuiet(t, f)
}

// From set_info_async_test.go.
func assertSetInfoBufferedResult(t *testing.T, f *setInfoFixture, class wire.FileInfoClass, data []byte) {
	t.Helper()
	// A later flush must not restore the buffered size or overwrite explicit times.
	if err := f.storage.Flush(f.ctx, f.open.Handle, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	attr := f.attr(t)
	wantSize := uint64(len(data))
	switch uint8(class) {
	case uint8(wire.ClassFileBasic):
		want := time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC)
		if !attr.Accessed.Equal(want) || !attr.Modified.Equal(want) || !attr.Changed.Equal(want) {
			t.Fatalf("explicit times lost after flush: %+v", attr)
		}
	case uint8(wire.ClassFileEndOfFile):
		wantSize = 5000
	case uint8(wire.ClassFileAllocation):
		wantSize = 4096
	default:
		t.Fatalf("unexpected metadata class: %d", class)
	}
	if attr.Size != wantSize {
		t.Fatalf("EOF after SET_INFO and flush = %d, want %d", attr.Size, wantSize)
	}
	got := make([]byte, wantSize)
	count, err := f.storage.ReadAt(f.ctx, f.open.Handle, got, 0)
	if err != nil || count != len(got) || !bytes.Equal(got, data[:wantSize]) {
		t.Fatalf("retained data: count %d, error %v, matches %t", count, err, bytes.Equal(got, data[:wantSize]))
	}
}

// From set_info_namespace_cancel_test.go.
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

// From set_info_namespace_retry_test.go.
// lookupMoveStorage changes real namespace entries after the discovery lookup.
// Data, attributes and every other operation still use the real adapter.
type lookupMoveStorage struct {
	smb.Storage
	move func(context.Context) error
	path string
	once sync.Once
}

// From set_info_namespace_retry_test.go.
func (s *lookupMoveStorage) Lookup(ctx context.Context, path string) (smb.Resolved, error) {
	resolved, err := s.Storage.Lookup(ctx, path)
	if err != nil || path != s.path {
		return resolved, err
	}
	var moveErr error
	s.once.Do(func() { moveErr = s.move(ctx) })
	if moveErr != nil {
		return smb.Resolved{}, moveErr
	}
	return resolved, nil
}

// From set_info_namespace_test.go.
const namespaceDeleteAccess uint32 = 0x00010000

// From set_info_namespace_test.go.
// namespaceClient uses real storage and sends every mutation through SET_INFO.
type namespaceClient struct {
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	nextID  uint64
}

// From set_info_namespace_test.go.
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

// From set_info_namespace_test.go.
func (f *namespaceClient) binding() state.Binding {
	return state.Binding{SessionID: f.session.SessionID, TreeID: f.session.TreeID}
}

// From set_info_namespace_test.go.
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

// From set_info_namespace_test.go.
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

// From set_info_namespace_test.go.
func namespaceStatus(t *testing.T, got, want smb.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %#x, want %#x", got, want)
	}
}

// From set_info_namespace_test.go.
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

// From set_info_namespace_test.go.
func (f *namespaceClient) rename(t *testing.T, open state.Open, path string, replace bool) smb.Status {
	t.Helper()
	buffer, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: path, ReplaceIfExists: replace})
	if err != nil {
		t.Fatal(err)
	}
	return f.set(t, open, wire.ClassFileRename, buffer)
}

// From set_info_namespace_test.go.
func (f *namespaceClient) disposition(t *testing.T, open state.Open, pending bool) smb.Status {
	t.Helper()
	buffer, err := wire.EncodeFileDispositionInformation(wire.FileDispositionInformation{DeletePending: pending})
	if err != nil {
		t.Fatal(err)
	}
	return f.set(t, open, wire.ClassFileDisposition, buffer)
}

// From set_info_namespace_test.go.
func (f *namespaceClient) write(t *testing.T, open state.Open, data string) {
	t.Helper()
	if n, err := f.server.options.Storage.WriteAt(f.ctx, open.Handle, []byte(data), 0); err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
}

// From set_info_namespace_test.go.
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

// From set_info_namespace_test.go.
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

// From set_info_namespace_test.go.
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

// From set_info_test.go.
type setInfoFixture struct {
	storage smb.Storage
	client  *smbtest.Client
	ctx     context.Context
	open    state.Open
	session smbtest.Session
	nextID  uint64
}

// From set_info_test.go.
func newSetInfoFixture(t *testing.T, access uint32) *setInfoFixture {
	t.Helper()
	return newSetInfoFixtureForPath(t, "data", access)
}

// From set_info_test.go.
func newSetInfoFixtureForPath(t *testing.T, path string, access uint32) *setInfoFixture {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := newFilesMetaClient(t, server)
	open := insertFilesMetaOpen(t, server, session, path, access)
	return &setInfoFixture{storage: options.Storage, client: client, ctx: ctx, session: session, open: open, nextID: session.NextMessageID}
}

// From set_info_test.go.
func (f *setInfoFixture) set(t *testing.T, info wire.SetInfoRequest, want smb.Status) {
	t.Helper()
	body, err := wire.EncodeSetInfoRequest(info)
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(f.ctx, t, f.client, wire.Message{Header: wire.Header{
		Command: wire.SetInfo, MessageID: f.nextID, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: 1, Credit: 16,
	}, Body: body})[0]
	if response.Header.Status == smb.StatusPending {
		final, receiveErr := f.client.Receive(f.ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if len(final.Messages) != 1 {
			t.Fatalf("SET_INFO final reply contains %d messages, want one", len(final.Messages))
		}
		response = final.Messages[0]
	}
	f.nextID++
	if response.Header.Status != want {
		t.Fatalf("SET_INFO type %d class %d: status %#x, want %#x", info.InfoType, info.InfoClass, response.Header.Status, want)
	}
	if want == smb.StatusSuccess {
		if _, decodeErr := wire.DecodeSetInfoResponse(response); decodeErr != nil {
			t.Fatal(decodeErr)
		}
	}
}

// From set_info_test.go.
func (f *setInfoFixture) basic(t *testing.T, info wire.FileBasicInformation, want smb.Status) {
	t.Helper()
	buffer, err := wire.EncodeFileBasicInformation(info)
	if err != nil {
		t.Fatal(err)
	}
	f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileBasic), Input: buffer}, want)
}

// From set_info_test.go.
func (f *setInfoFixture) size(t *testing.T, class wire.FileInfoClass, size uint64, want smb.Status) {
	t.Helper()
	var buffer []byte
	var err error
	if class == wire.ClassFileEndOfFile {
		buffer, err = wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: size})
	} else {
		buffer, err = wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: size})
	}
	if err != nil {
		t.Fatal(err)
	}
	f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(class), Input: buffer}, want)
}

// From set_info_test.go.
func (f *setInfoFixture) attr(t *testing.T) smb.Attr {
	t.Helper()
	attr, err := f.storage.GetAttr(f.ctx, f.open.Object)
	if err != nil {
		t.Fatal(err)
	}
	return attr
}

// From set_info_test.go.
func (f *setInfoFixture) write(t *testing.T, data string) {
	t.Helper()
	count, err := f.storage.WriteAt(f.ctx, f.open.Handle, []byte(data), 0)
	if err != nil || count != len(data) {
		t.Fatalf("WriteAt = %d, %v", count, err)
	}
}

// From set_info_test.go.
func filetime(t *testing.T, value time.Time) wire.Filetime {
	t.Helper()
	encoded, err := wire.EncodeFiletime(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// From sharing_test.go.
type sharingClient struct {
	ctx     context.Context
	client  *smbtest.Client
	done    chan error
	session smbtest.Session
	next    uint64
}

// From sharing_test.go.
func newSharingServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return server
}

// From sharing_test.go.
func newSharingClient(t *testing.T, server *Server) *sharingClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	peer := &sharingClient{client: client, ctx: ctx, done: make(chan error, 1)}
	go func() { peer.done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() { peer.drop(t); cancel() })
	peer.session, err = client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningCMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	peer.next = peer.session.NextMessageID
	return peer
}

// From sharing_test.go.
func (peer *sharingClient) drop(t *testing.T) {
	t.Helper()
	if peer.done == nil {
		return
	}
	if err := peer.client.Close(); err != nil {
		t.Error(err)
	}
	select {
	case err := <-peer.done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sharing connection did not stop")
	}
	peer.done = nil
}

// From sharing_test.go.
func (peer *sharingClient) create(t *testing.T, request wire.CreateRequest) wire.Message {
	t.Helper()
	response := fileCreate(peer.ctx, t, peer.client, peer.session, peer.next, request)
	peer.next++
	return response
}

// From sharing_test.go.
func (peer *sharingClient) open(t *testing.T, name string, access, sharing uint32) wire.FileID {
	t.Helper()
	return createdFile(t, peer.create(t, wire.CreateRequest{Name: name, DesiredAccess: access, ShareAccess: sharing, Disposition: fileOpenIf})).ID
}

// From sharing_test.go.
func (peer *sharingClient) close(t *testing.T, id wire.FileID) {
	t.Helper()
	response := fileClose(peer.ctx, t, peer.client, peer.session, peer.next, id, 0)
	peer.next++
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE status = %#x", response.Header.Status)
	}
}

// From sharing_test.go.
func sharingAccess(rights uint32) uint32 {
	access := rights & 3
	if rights&4 != 0 {
		access |= fileDelete
	}
	return access
}

// From sharing_test.go.
// MS-FSA 2.1.5.1.2.2 compares both directions, but only if both opens
// request data, execute, append or delete access.
func expectedSharing(firstAccess, firstShare, secondAccess, secondShare uint32) smb.Status {
	if firstAccess == 0 || secondAccess == 0 {
		return smb.StatusSuccess
	}
	if secondAccess&^firstShare != 0 || firstAccess&^secondShare != 0 {
		return smb.StatusSharingViolation
	}
	return smb.StatusSuccess
}

// From sharing_test.go.
func checkSharingPair(t *testing.T, first, second *sharingClient, firstAccess, firstShare, secondAccess, secondShare uint32) {
	t.Helper()
	id := first.open(t, "matrix", sharingAccess(firstAccess), firstShare)
	response := second.create(t, wire.CreateRequest{Name: "matrix", DesiredAccess: sharingAccess(secondAccess), ShareAccess: secondShare, Disposition: fileOpen})
	want := expectedSharing(firstAccess, firstShare, secondAccess, secondShare)
	if response.Header.Status != want {
		t.Fatalf("first access/share %d/%d, second %d/%d: status %#x, want %#x", firstAccess, firstShare, secondAccess, secondShare, response.Header.Status, want)
	}
	if want == smb.StatusSuccess {
		second.close(t, createdFile(t, response).ID)
	}
	first.close(t, id)
	// Neither a rejected CREATE nor CLOSE may leave sharing behind.
	compatible := second.open(t, "matrix", sharingAccess(7), 0)
	second.close(t, compatible)
}

// From sharing_test.go.
func (peer *sharingClient) exchange(t *testing.T, command wire.Command, body []byte) wire.Message {
	t.Helper()
	response := ioRoundTrip(peer.ctx, t, peer.client, ioMessage(peer.session, peer.next, command, body, 1))
	peer.next++
	return response
}

// From sharing_test.go.
func (peer *sharingClient) write(t *testing.T, id wire.FileID, data []byte) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	response := peer.exchange(t, wire.Write, body)
	requireIOStatus(t, response, smb.StatusSuccess)
	written, err := wire.DecodeWriteResponse(response)
	if err != nil || uint64(written.Count) != uint64(len(data)) {
		t.Fatalf("WRITE: %+v, %v", written, err)
	}
}

// From sharing_test.go.
func (peer *sharingClient) read(t *testing.T, id wire.FileID, want string) {
	t.Helper()
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: smb.CreditUnit})
	if err != nil {
		t.Fatal(err)
	}
	response := peer.exchange(t, wire.Read, body)
	requireIOStatus(t, response, smb.StatusSuccess)
	read, err := wire.DecodeReadResponse(response)
	if err != nil || string(read.Data) != want {
		t.Fatalf("READ data = %q, want %q, error %v", read.Data, want, err)
	}
}

// From sharing_test.go.
type sharingFailureStorage struct {
	smb.Storage
	closed   atomic.Uint64
	failAttr atomic.Bool
}

// From sharing_test.go.
func (storage *sharingFailureStorage) GetAttr(ctx context.Context, key smb.ObjectKey) (smb.Attr, error) {
	if storage.failAttr.Swap(false) {
		return smb.Attr{}, smb.ErrIO
	}
	return storage.Storage.GetAttr(ctx, key)
}

// From sharing_test.go.
func (storage *sharingFailureStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.closed.Add(1)
	return storage.Storage.Close(ctx, handle)
}

// From shutdown_test.go.
type blockedCleanup struct {
	*cleanupStorage
	entered chan struct{}
	release chan struct{}
}

// From shutdown_test.go.
func (storage *blockedCleanup) Close(ctx context.Context, handle smb.Handle) error {
	close(storage.entered)
	<-storage.release
	if err := ctx.Err(); err != nil {
		return err
	}
	return storage.cleanupStorage.Close(ctx, handle)
}

// From smbclient_test.go.
func checkSmbclientLogin(t *testing.T, encrypted bool) {
	t.Helper()
	options := testOptions(t)
	protection := "encrypt"
	if !encrypted {
		options.Encryption = AllowPlaintext
		protection = "sign"
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if shutdownErr := server.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
			t.Error(shutdownErr)
		}
		if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			t.Error(serveErr)
		}
	})
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "smbclient", "//"+host+"/"+options.ShareName, "-p", port, "-U", options.Account.User, //nolint:gosec // Arguments use fixed test credentials and the test's loopback listener.
		"--option=client min protocol=SMB3_11", "--option=client max protocol=SMB3_11", "--client-protection="+protection, "-c", "quit")
	command.Env = append(os.Environ(), "PASSWD="+options.Account.Password)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("smbclient login: %v\n%s", err, output)
	}
}

// From stream_fuzz_test.go.
const streamBound = 3 * time.Second

// From stream_fuzz_test.go.
func runFuzzInput(t *testing.T, storage smb.Storage, root smb.Attr, stream []byte) {
	t.Helper()
	// Bound total work as well as individual frame allocations.
	if len(stream) > 64<<10 {
		t.Skip("stream exceeds corpus limit")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), streamBound)
		defer cancel()
		if err := resetFuzzStorage(ctx, storage, root); err != nil {
			t.Error(err)
		}
	})
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup is LIFO: server requests and opens drain before storage resets.
	runServerStream(t, server, stream)
}

// From stream_fuzz_test.go.
type streamSeed struct {
	name        string
	stream      []byte
	wantReplies int
}

// From stream_fuzz_test.go.
func streamSeeds(t testing.TB) []streamSeed {
	t.Helper()
	negotiate := streamFrame(t, negotiateMessage(t, 16))
	first := streamFrame(t, echo(t, 1))
	second := streamFrame(t, echo(t, 2))
	compound := streamFrame(t, echo(t, 1), echo(t, 2))
	setup := streamFrame(t, sessionSetupSeed(t))
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\server\\backup"})
	if err != nil {
		t.Fatal(err)
	}
	// Session 1 is in progress after the first NTLM step. This seed checks
	// the tree request decoder and session check, not an authenticated tree.
	tree := streamFrame(t, wire.Message{Header: wire.Header{Command: wire.TreeConnect, MessageID: 2, SessionID: 1, CreditCharge: 1, Credit: 16}, Body: body})
	return []streamSeed{
		{name: "negotiate", stream: negotiate, wantReplies: 1},
		{name: "echo stream", stream: bytes.Join([][]byte{negotiate, first, second}, nil), wantReplies: 3},
		{name: "echo compound", stream: bytes.Join([][]byte{negotiate, compound}, nil), wantReplies: 3},
		{name: "session setup", stream: bytes.Join([][]byte{negotiate, setup}, nil), wantReplies: 2},
		{name: "tree connect", stream: bytes.Join([][]byte{negotiate, setup, tree}, nil), wantReplies: 3},
	}
}

// From stream_fuzz_test.go.
func sessionSetupSeed(t testing.TB) wire.Message {
	t.Helper()
	account := auth.Account{User: "backup", Password: "password"}
	acceptor, err := auth.NewAcceptor(auth.Options{Account: account, ServerName: "s3-smb"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	initiator, err := auth.NewInitiator(account, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initiator.Start(token)
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, SecurityMode: 3})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: 1, CreditCharge: 1, Credit: 16}, Body: body}
}

// From stream_fuzz_test.go.
func streamFrame(t testing.TB, messages ...wire.Message) []byte {
	t.Helper()
	payload, err := wire.Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 4, 4+len(payload))
	if len(payload) > 0xffffff {
		t.Fatal("seed frame exceeds direct TCP length")
	}
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	return append(frame, payload...)
}

// From stream_fuzz_test.go.
func runServerStream(t *testing.T, server *Server, stream []byte) ([]wire.Message, bool) {
	t.Helper()
	// The test context is canceled before cleanup, but the peer-close wait must not be.
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if err := ctx.Err(); err != nil {
			t.Errorf("server context canceled before peer-close wait: %v", err)
		}
		select {
		case err := <-done:
			// Protocol rejection is an expected outcome for mutated inputs.
			if err != nil {
				t.Logf("ServeConn: %v", err)
			}
		case <-time.After(streamBound):
			t.Error("ServeConn did not stop within the bound")
		}
		cancel()
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), streamBound)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Error(err)
		}
	})
	replies, closed, err := feedStream(remote, stream, streamBound)
	if err != nil {
		t.Fatal(err)
	}
	return replies, closed
}

// From stream_fuzz_test.go.
// feedStream preserves frame bytes and connection state between requests.
// One deadline bounds the whole stream, not each frame.
// A complete request must get a framed reply or a peer close. CANCEL has no
// reply. An incomplete final frame is followed by a client close, not a read
// deadline: the server cannot answer bytes it has not received.
func feedStream(conn net.Conn, stream []byte, bound time.Duration) (replies []wire.Message, closed bool, err error) {
	defer func() { err = errors.Join(err, conn.Close()) }()
	deadlineErr := conn.SetDeadline(time.Now().Add(bound))
	if streamPeerClosed(deadlineErr) {
		return nil, true, nil
	}
	if deadlineErr != nil {
		return nil, false, deadlineErr
	}
	for len(stream) > 0 {
		length := len(stream)
		complete := false
		if len(stream) >= 4 {
			length = 4 + int(stream[1])<<16 + int(stream[2])<<8 + int(stream[3])
			complete = length <= len(stream)
			length = min(length, len(stream))
		}
		frame := stream[:length]
		messages, peerClosed, exchangeErr := exchangeStreamFrame(conn, frame, complete && needsStreamReply(frame))
		if exchangeErr != nil {
			return replies, false, exchangeErr
		}
		replies = append(replies, messages...)
		if peerClosed || !complete {
			return replies, peerClosed, nil
		}
		stream = stream[length:]
	}
	return replies, false, nil
}

// From stream_fuzz_test.go.
func needsStreamReply(frame []byte) bool {
	messages, err := wire.Split(frame[4:])
	if err != nil {
		return true
	}
	for _, message := range messages {
		if message.Header.Command != wire.Cancel {
			return true
		}
	}
	return false
}

// From stream_fuzz_test.go.
func exchangeStreamFrame(conn net.Conn, frame []byte, wantReply bool) ([]wire.Message, bool, error) {
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, bytes.NewReader(frame))
		written <- err
	}()
	var messages []wire.Message
	var readErr error
	if wantReply {
		messages, readErr = readStreamReplies(conn, frame)
		if readErr != nil {
			// Unblock the writer before waiting for its result.
			if err := conn.Close(); err != nil {
				return nil, false, errors.Join(readErr, err, <-written)
			}
		}
	}
	writeErr := <-written
	if readErr != nil && !streamPeerClosed(readErr) {
		return nil, false, readErr
	}
	if writeErr != nil && !streamPeerClosed(writeErr) {
		return nil, false, fmt.Errorf("write request: %w", writeErr)
	}
	peerClosed := streamPeerClosed(readErr) || streamPeerClosed(writeErr)
	if peerClosed {
		return nil, true, nil
	}
	if !wantReply {
		return nil, false, nil
	}
	return messages, false, nil
}

// From stream_fuzz_test.go.
// Valid compounds may produce separate prefix, interim and final frames. Count
// terminal replies by request identity, not frames or the number of interims.
func readStreamReplies(conn net.Conn, frame []byte) ([]wire.Message, error) {
	requests, decodeErr := wire.Split(frame[4:])
	known := decodeErr == nil
	remaining := make(map[uint64]struct{})
	for _, request := range requests {
		if request.Header.Command != wire.Cancel {
			remaining[request.Header.MessageID] = struct{}{}
		}
	}
	pending := make(map[uint64]wire.Header)
	var replies []wire.Message
	for {
		reader := &streamReplyReader{Reader: conn}
		payload, err := readFrame(reader, max(smb.MaxTransactSize, smb.MaxReadSize, smb.MaxWriteSize)+smb.CreditUnit)
		if streamPeerClosed(err) && (reader.started || len(replies) != 0) {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, fmt.Errorf("read reply: %w", err)
		}
		messages, err := wire.Split(payload)
		if err != nil {
			return nil, fmt.Errorf("invalid reply: %w", err)
		}
		for _, message := range messages {
			if err := countStreamReply(message.Header, remaining, pending, known); err != nil {
				return nil, err
			}
		}
		replies = append(replies, messages...)
		if !known || len(remaining) == 0 {
			return replies, nil
		}
	}
}

// From stream_fuzz_test.go.
func countStreamReply(header wire.Header, remaining map[uint64]struct{}, pending map[uint64]wire.Header, known bool) error {
	if header.Flags&wire.FlagResponse == 0 {
		return errors.New("reply lacks response flag")
	}
	if !known {
		return nil
	}
	if _, exists := remaining[header.MessageID]; !exists {
		return errors.New("reply has unexpected or completed message ID")
	}
	interim, waiting := pending[header.MessageID]
	if header.Status == smb.StatusPending {
		if waiting || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 {
			return errors.New("invalid interim reply")
		}
		pending[header.MessageID] = header
		return nil
	}
	if waiting {
		if header.Flags&wire.FlagAsync == 0 || header.AsyncID != interim.AsyncID || header.SessionID != interim.SessionID || header.Command != interim.Command || header.Credit != 0 {
			return errors.New("invalid final async reply")
		}
		delete(pending, header.MessageID)
	} else if header.Flags&wire.FlagAsync != 0 {
		return errors.New("async reply without interim")
	}
	delete(remaining, header.MessageID)
	return nil
}

// From stream_fuzz_test.go.
type streamReplyReader struct {
	io.Reader
	started bool
}

// From stream_fuzz_test.go.
func (reader *streamReplyReader) Read(buffer []byte) (int, error) {
	n, err := reader.Reader.Read(buffer)
	reader.started = reader.started || n > 0
	return n, err
}

// From stream_fuzz_test.go.
func streamPeerClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed)
}

// From stream_pending_test.go.
func streamAsyncReply(t *testing.T, id uint64, status smb.Status) wire.Message {
	t.Helper()
	body, err := wire.EncodeErrorResponse(wire.ErrorResponse{})
	if err != nil {
		t.Fatal(err)
	}
	credit := uint16(0)
	if status == smb.StatusPending {
		credit = 3
	}
	return wire.Message{Header: wire.Header{Command: wire.Read, MessageID: id, SessionID: 77, AsyncID: id + 10, Flags: wire.FlagResponse | wire.FlagAsync, Status: status, Credit: credit}, Body: body}
}

// From stream_test.go.
func checkStreamSeedReply(t *testing.T, id uint64, message wire.Message) {
	t.Helper()
	want := smb.StatusSuccess
	var err error
	switch uint16(message.Header.Command) {
	case uint16(wire.Negotiate):
		_, err = wire.DecodeNegotiateResponse(message)
	case uint16(wire.Echo):
		_, err = wire.DecodeEchoResponse(message)
	case uint16(wire.SessionSetup):
		want = smb.StatusMoreProcessingRequired
		_, err = wire.DecodeSessionSetupResponse(message)
		if message.Header.SessionID == 0 {
			t.Fatal("session seed did not start authentication")
		}
	case uint16(wire.TreeConnect):
		want = smb.StatusUserSessionDeleted
		_, err = wire.DecodeErrorResponse(message)
	default:
		t.Fatal("unexpected seed command", message.Header.Command)
	}
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Status != want || message.Header.MessageID != id {
		t.Fatalf("reply %d: %+v", id, message.Header)
	}
}

// From stream_test.go.
func streamPeer(t *testing.T, serve func(net.Conn) error) net.Conn {
	t.Helper()
	local, remote := net.Pipe()
	done := make(chan error, 1)
	go func() {
		err := serve(remote)
		done <- errors.Join(err, remote.Close())
	}()
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(streamBound):
			t.Error("test peer did not stop")
		}
	})
	return local
}

// From streams_info_test.go.
func assertStreamList(t *testing.T, data []byte, want map[string]uint64) {
	t.Helper()
	listed, err := wire.DecodeFileStreamInformation(data)
	if err != nil {
		t.Fatal(err)
	}
	want = maps.Clone(want)
	if len(listed.Entries) != len(want) {
		t.Fatalf("streams: %+v", listed.Entries)
	}
	for _, entry := range listed.Entries {
		size, exists := want[entry.Name]
		if !exists || size != entry.Size || entry.AllocationSize < size {
			t.Fatalf("unexpected stream: %+v", entry)
		}
		delete(want, entry.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing streams: %v", want)
	}
}

// From streams_info_test.go.
func assertStreamLength(t *testing.T, data []byte, class wire.FileInfoClass, want uint64) {
	t.Helper()
	var size, allocation uint64
	switch uint8(class) {
	case uint8(wire.ClassFileStandard):
		info, err := wire.DecodeFileStandardInformation(data)
		if err != nil {
			t.Fatal(err)
		}
		size, allocation = info.EndOfFile, info.AllocationSize
		if info.Directory {
			t.Fatal("stream reported as directory")
		}
	case uint8(wire.ClassFileAll):
		info, err := wire.DecodeFileAllInformation(data)
		if err != nil {
			t.Fatal(err)
		}
		size, allocation = info.Standard.EndOfFile, info.Standard.AllocationSize
	case uint8(wire.ClassFileNetworkOpen):
		info, err := wire.DecodeFileNetworkOpenInformation(data)
		if err != nil {
			t.Fatal(err)
		}
		size, allocation = info.EndOfFile, info.AllocationSize
	default:
		t.Fatalf("unexpected class %d", class)
	}
	if size != want || allocation < size {
		t.Fatalf("class %d: EOF %d allocation %d, want EOF %d", class, size, allocation, want)
	}
}

// From streams_test.go.
// These tests exercise named objects through protected raw SMB messages, not
// adapter calls. The storage fixture uses SQLite and file-backed JuiceFS data.
type streamClient struct {
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

// From streams_test.go.
func newStreamClient(t *testing.T) *streamClient {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := newFilesMetaClient(t, server)
	return &streamClient{server: server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

// From streams_test.go.
func (c *streamClient) call(t *testing.T, command wire.Command, body []byte) wire.Message {
	t.Helper()
	message := wire.Message{Header: wire.Header{Command: command, MessageID: c.next, SessionID: c.session.SessionID, TreeID: c.session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
	c.next++
	if err := c.client.Send(c.ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	for {
		reply, err := c.client.Receive(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Messages) != 1 || reply.Messages[0].Header.MessageID != message.Header.MessageID {
			t.Fatalf("unexpected reply: %+v", reply.Messages)
		}
		if reply.Messages[0].Header.Status != smb.StatusPending {
			return reply.Messages[0]
		}
	}
}

// From streams_test.go.
func streamRequest(name string, disposition uint32) wire.CreateRequest {
	return wire.CreateRequest{Name: name, Disposition: disposition, DesiredAccess: 0x10000000, ShareAccess: 7} // GENERIC_ALL.
}

// From streams_test.go.
func (c *streamClient) create(t *testing.T, request wire.CreateRequest, status smb.Status) wire.CreateResponse {
	t.Helper()
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Create, body)
	if reply.Header.Status != status {
		t.Fatalf("CREATE %q disposition %d: status %x, want %x", request.Name, request.Disposition, reply.Header.Status, status)
	}
	if status != smb.StatusSuccess {
		return wire.CreateResponse{}
	}
	result, err := wire.DecodeCreateResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// From streams_test.go.
func (c *streamClient) close(t *testing.T, id wire.FileID) wire.CloseResponse {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Close, body)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE: %x", reply.Header.Status)
	}
	result, err := wire.DecodeCloseResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// From streams_test.go.
func (c *streamClient) write(t *testing.T, id wire.FileID, data []byte, offset uint64, status smb.Status) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data, Offset: offset})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Write, body)
	if reply.Header.Status != status {
		t.Fatalf("WRITE at %d: %x, want %x", offset, reply.Header.Status, status)
	}
	if status != smb.StatusSuccess {
		return
	}
	result, err := wire.DecodeWriteResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(result.Count) != uint64(len(data)) {
		t.Fatalf("WRITE count: %d, want %d", result.Count, len(data))
	}
}

// From streams_test.go.
func (c *streamClient) read(t *testing.T, id wire.FileID, offset uint64, want []byte) {
	t.Helper()
	length := uint64(len(want))
	if length > 65536 {
		t.Fatal("stream test read exceeds one credit")
		return
	}
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Offset: offset, Length: uint32(length)})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Read, body)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("READ at %d: %x", offset, reply.Header.Status)
	}
	result, err := wire.DecodeReadResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Data, want) {
		t.Fatalf("READ at %d: got %q, want %q", offset, result.Data, want)
	}
}

// From streams_test.go.
func (c *streamClient) resize(t *testing.T, id wire.FileID, size uint64, status smb.Status) {
	t.Helper()
	input, err := wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: size})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileEndOfFile), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.SetInfo, body)
	if reply.Header.Status != status {
		t.Fatalf("SET_INFO EOF %d: %x, want %x", size, reply.Header.Status, status)
	}
}

// From streams_test.go.
func (c *streamClient) query(t *testing.T, id wire.FileID, class wire.FileInfoClass) []byte {
	t.Helper()
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), OutputLength: 4096})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.QueryInfo, body)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("QUERY_INFO class %d: %x", class, reply.Header.Status)
	}
	result, err := wire.DecodeQueryInfoResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	return result.Data
}

// From streams_test.go.
func testStreamDisposition(t *testing.T, c *streamClient, stream string, disposition uint32, presence string) {
	t.Helper()
	base := fmt.Sprintf("data-%s-%d-%s", stream, disposition, presence)
	baseData := []byte("base bytes must survive every stream disposition")
	var baseID wire.FileID
	if presence != "no-base" {
		baseID = c.create(t, streamRequest(base, fileCreateDisposition), smb.StatusSuccess).ID
		c.write(t, baseID, baseData, 0, smb.StatusSuccess)
	}
	name := base + ":" + stream + ":$DATA"
	if presence == "stream" {
		id := c.create(t, streamRequest(name, fileCreateDisposition), smb.StatusSuccess).ID
		c.write(t, id, []byte("old stream"), 0, smb.StatusSuccess)
		c.close(t, id)
	}
	status, action := streamDispositionResult(disposition, presence)
	opened := c.create(t, streamRequest(name, disposition), status)
	if status == smb.StatusSuccess {
		if opened.Action != action {
			t.Fatalf("action: %d, want %d", opened.Action, action)
		}
		wantSize := uint64(0)
		if presence == "stream" && (disposition == fileOpen || disposition == fileOpenIf) {
			wantSize = 10
			c.read(t, opened.ID, 0, []byte("old stream"))
		}
		if opened.Size != wantSize {
			t.Fatalf("CREATE EOF: %d, want %d", opened.Size, wantSize)
		}
		if closed := c.close(t, opened.ID); closed.Size != wantSize {
			t.Fatalf("CLOSE EOF: %d, want %d", closed.Size, wantSize)
		}
	}
	if status == smb.StatusObjectNameCollision {
		id := c.create(t, streamRequest(name, fileOpen), smb.StatusSuccess).ID
		c.read(t, id, 0, []byte("old stream"))
		c.close(t, id)
	}
	if presence == "no-base" {
		c.create(t, streamRequest(base, fileOpen), smb.StatusObjectNameNotFound)
	} else {
		c.read(t, baseID, 0, baseData)
		c.close(t, baseID)
	}
}

// From streams_test.go.
func streamDispositionResult(disposition uint32, presence string) (smb.Status, uint32) {
	if presence == "no-base" {
		return smb.StatusObjectNameNotFound, 0
	}
	if presence == "base-only" {
		if disposition == 1 || disposition == 4 {
			return smb.StatusObjectNameNotFound, 0
		}
		return smb.StatusSuccess, 2 // FILE_CREATED.
	}
	switch disposition {
	case 0:
		return smb.StatusSuccess, 0 // FILE_SUPERSEDED.
	case 1, 3:
		return smb.StatusSuccess, 1 // FILE_OPENED.
	case 2:
		return smb.StatusObjectNameCollision, 0
	default:
		return smb.StatusSuccess, 3 // FILE_OVERWRITTEN.
	}
}
