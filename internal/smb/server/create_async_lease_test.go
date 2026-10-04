package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
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
