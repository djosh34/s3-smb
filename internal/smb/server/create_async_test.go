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
