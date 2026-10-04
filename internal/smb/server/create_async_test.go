package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Controlled handlers test transport progress, not lease policy or storage.
func TestPendingCreateAllowsHolderProgress(t *testing.T) {
	for _, command := range []wire.Command{wire.OplockBreak, wire.Close} {
		for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
			t.Run(fmt.Sprintf("command_%d_cipher_%d", command, cipher), func(t *testing.T) {
				checkPendingCreateProgress(t, command, cipher)
			})
		}
	}
}

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
		if err := server.sendLeaseBreak(ctx, state.Break{Binding: request.Binding(), ClientGUID: request.Session.ClientGUID, LeaseKey: state.GUID(key), CurrentState: 7, AckRequired: true, Epoch: 1}); err != nil {
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

func TestLocalCreateGetsOneSynchronousReply(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	id := wire.FileID{Persistent: 30, Volatile: 40}
	result := createTestReply(t, id)
	server.handlers[wire.Create] = func(context.Context, RequestContext, wire.Message) (reply, error) {
		return result, nil
	}
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	request := asyncMessage(t, wire.Create, 1)
	responses := exchange(ctx, t, client, request)
	if len(responses) != 1 {
		t.Fatalf("local CREATE replies: %+v", responses)
	}
	response := responses[0]
	header := response.Header
	if header.Command != wire.Create || header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 || header.MessageID != 1 || header.SessionID != 77 || header.TreeID != 12 || header.Credit != 16 || header.CreditCharge != 1 {
		t.Fatalf("local CREATE identity/credits: %+v", header)
	}
	assertCreatedFileID(t, response, id)
	assertCreateTestEcho(ctx, t, client, smbtest.Session{SessionID: 77}, 2)
}

func TestPendingCreateRelatedCloseInheritsFileID(t *testing.T) {
	id := wire.FileID{Persistent: 30, Volatile: 40}
	server, release := controlledAsync(t, wire.Create, createTestReply(t, id), nil)
	used := make(chan wire.FileID, 1)
	server.handlers[wire.Close] = func(_ context.Context, request RequestContext, message wire.Message) (reply, error) {
		decoded, err := wire.DecodeCloseRequest(message)
		if err != nil {
			return reply{}, err
		}
		resolved, status := request.FileID(decoded.ID)
		if status != smb.StatusSuccess {
			return reply{status: status}, nil
		}
		if request.Session.SessionID != 77 || request.Tree.TreeID != 12 {
			return reply{}, errors.New("related CLOSE lost session or tree identity")
		}
		used <- resolved
		body, err := wire.EncodeCloseResponse(wire.CloseResponse{})
		return reply{body: body, fileID: resolved}, err
	}
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 3))
	session := smbtest.Session{SessionID: 77, TreeID: 12}
	prefix := sessionEcho(t, session, 1)
	create := asyncMessage(t, wire.Create, 2)
	closeRequest := compoundFileRequest(t, session, wire.Close, 3, placeholderFileID(), true)
	if err := client.Send(ctx, []wire.Message{prefix, create, closeRequest}); err != nil {
		t.Fatal(err)
	}
	response := receiveCreateTestMessage(ctx, t, client)
	if response.Header.Command != wire.Echo || response.Header.MessageID != 1 || response.Header.Status != smb.StatusSuccess || response.Header.Flags&wire.FlagAsync != 0 || response.Header.Credit != 1 {
		t.Fatalf("completed prefix: %+v", response.Header)
	}
	createPending := receiveCreateTestMessage(ctx, t, client)
	assertCreatePending(t, create, createPending)
	closePending := receiveCreateTestMessage(ctx, t, client)
	// Expected identity is resolved, not the related wire placeholders.
	closeRequest.Header.SessionID, closeRequest.Header.TreeID = session.SessionID, session.TreeID
	assertCreatePending(t, closeRequest, closePending)
	if closePending.Header.AsyncID == createPending.Header.AsyncID {
		t.Fatal("related members share an async ID")
	}
	assertCreateTestEcho(ctx, t, client, session, 4)
	select {
	case got := <-used:
		t.Fatalf("related CLOSE ran before CREATE completed: %+v", got)
	default:
	}
	close(release)
	seen := make(map[uint64]bool)
	for range 2 {
		final := receiveCreateTestMessage(ctx, t, client)
		messageID := final.Header.MessageID
		if seen[messageID] {
			t.Fatalf("duplicate compound final: %+v", final.Header)
		}
		seen[messageID] = true
		switch messageID {
		case 2:
			assertCreateFinal(t, create, createPending, final, smb.StatusSuccess)
			assertCreatedFileID(t, final, id)
		case 3:
			assertCreateFinal(t, closeRequest, closePending, final, smb.StatusSuccess)
			if _, err := wire.DecodeCloseResponse(final); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected compound final: %+v", final.Header)
		}
	}
	select {
	case got := <-used:
		if got != id {
			t.Fatalf("related CLOSE FileId: %+v, want %+v", got, id)
		}
	default:
		t.Fatal("related CLOSE did not use the created FileId")
	}
	assertCreateTestEcho(ctx, t, client, session, 5)
}

func TestCancelAffectsOnlyItsPendingCreate(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", async), func(t *testing.T) {
			checkCreateCancellation(t, async)
		})
	}
}

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

func createTestReply(t *testing.T, id wire.FileID) reply {
	t.Helper()
	body, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return reply{body: body, fileID: id}
}

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

func assertCreatePending(t *testing.T, request, pending wire.Message) {
	t.Helper()
	header := pending.Header
	if header.Command != request.Header.Command || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || header.Credit != 16 || header.CreditCharge != 1 || header.TreeID != 0 {
		t.Fatalf("CREATE pending identity/credits: %+v", header)
	}
}

func assertCreateFinal(t *testing.T, request, pending, final wire.Message, status smb.Status) {
	t.Helper()
	header := final.Header
	if header.Command != request.Header.Command || header.MessageID != request.Header.MessageID || header.SessionID != pending.Header.SessionID || header.AsyncID != pending.Header.AsyncID || header.Flags&wire.FlagAsync == 0 || header.Status != status || header.Credit != 0 || header.CreditCharge != 1 || header.TreeID != 0 {
		t.Fatalf("CREATE final identity/credits: %+v", header)
	}
}

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

func assertCreateTestEcho(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64) {
	t.Helper()
	messages := exchange(ctx, t, client, sessionEcho(t, session, id))
	if len(messages) != 1 || messages[0].Header.Command != wire.Echo || messages[0].Header.MessageID != id || messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("extra reply or failed ECHO: %+v", messages)
	}
}
