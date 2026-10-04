package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
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
		if err := sendCreateTestBreak(request, key); err != nil {
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
	notification := receiveCreateTestMessage(ctx, t, client)
	if notification.Header.Command != wire.OplockBreak || notification.Header.MessageID != ^uint64(0) || notification.Header.SessionID != session.SessionID || notification.Header.Credit != 0 {
		t.Fatalf("lease notification: %+v", notification.Header)
	}
	decoded, err := wire.DecodeLeaseBreakNotification(notification)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Key != key || decoded.CurrentState != 7 || decoded.NewState != 0 || decoded.Flags != 1 {
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

func sendCreateTestBreak(request RequestContext, key [16]byte) error {
	body, err := wire.EncodeLeaseBreakNotification(wire.LeaseBreakNotification{Key: key, CurrentState: 7, Flags: 1})
	if err != nil {
		return err
	}
	request.server.mu.Lock()
	connection := request.server.sessions[request.Session.SessionID]
	request.server.mu.Unlock()
	if connection == nil {
		return errors.New("controlled lease holder has no connection")
	}
	return connection.send([]wire.Message{{Header: wire.Header{Command: wire.OplockBreak, MessageID: ^uint64(0), SessionID: request.Session.SessionID, Flags: wire.FlagResponse}, Body: body}})
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
