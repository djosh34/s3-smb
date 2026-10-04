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
	"net"
	"os"
	"os/exec"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

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

// From create_dispositions_test.go.
func (client *readWriteClient) create(t *testing.T, request wire.CreateRequest) wire.Message {
	t.Helper()
	message := fileCreate(client.ctx, t, client.client, client.session, client.next, request)
	client.next++
	return message
}

// From create_dispositions_test.go.
func createRequest(name string, disposition uint32) wire.CreateRequest {
	return wire.CreateRequest{Name: name, DesiredAccess: 0x10000000, ShareAccess: 7, Disposition: disposition}
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
