package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCompoundWarningAllowsClose(t *testing.T) {
	for _, status := range []smb.Status{smb.StatusBufferOverflow, smb.StatusNoMoreFiles} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%x_async_%t", status, async), func(t *testing.T) {
				checkCompoundWarningClose(t, status, async)
			})
		}
	}
}

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

func TestCompoundHandlerErrorPreservesFileID(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", async), func(t *testing.T) {
			checkCompoundHandlerErrorFileID(t, async)
		})
	}
}

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
