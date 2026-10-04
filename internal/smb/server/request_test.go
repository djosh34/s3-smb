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

func TestHandlerRequestContext(t *testing.T) {
	options := testOptions(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		if request.Storage != options.Storage || request.Opens != options.State {
			t.Error("handler did not receive shared modules")
		}
		if request.Session != (Session{}) || request.Tree != (Tree{}) {
			t.Error("unauthenticated ECHO has an identity")
		}
		return handleEcho(ctx, request, message)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	exchange(ctx, t, client, echo(t, 1))
}

func TestRequestFileID(t *testing.T) {
	placeholder := wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}
	id := wire.FileID{Persistent: 17, Volatile: 3}
	result := reply{fileID: id}
	for _, test := range []struct {
		name    string
		request RequestContext
		input   wire.FileID
		want    wire.FileID
		status  smb.Status
	}{
		{"existing", RequestContext{related: true, fileID: id}, id, id, smb.StatusSuccess},
		{"inherited", RequestContext{related: true, fileID: result.fileID}, placeholder, id, smb.StatusSuccess},
		{"missing", RequestContext{related: true}, placeholder, wire.FileID{}, smb.StatusInvalidParameter},
		{"unrelated", RequestContext{fileID: id}, placeholder, placeholder, smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, status := test.request.FileID(test.input)
			if got != test.want || status != test.status {
				t.Fatalf("FileID: %+v, %v; want %+v, %v", got, status, test.want, test.status)
			}
		})
	}
}

func TestHandlerCleanup(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("close failed")} {
		t.Run(fmt.Sprintf("status_%x", smb.StatusFromError(closeErr)), func(t *testing.T) {
			options := testOptions(t)
			storage := &cleanupStorage{closeErr: closeErr}
			options.Storage = storage
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			server.handlers[wire.Close] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
				decoded, decodeErr := wire.DecodeCloseRequest(message)
				if decodeErr != nil {
					return reply{}, decodeErr
				}
				action, status := request.Opens.Close(state.FileID{Persistent: decoded.ID.Persistent, Volatile: decoded.ID.Volatile}, request.Binding())
				if status != smb.StatusSuccess {
					return reply{status: status}, nil
				}
				if cleanupErr := request.Cleanup(ctx, []state.CloseAction{action}); cleanupErr != nil {
					return reply{}, cleanupErr
				}
				body, encodeErr := wire.EncodeCloseResponse(wire.CloseResponse{})
				return reply{body: body, fileID: decoded.ID}, encodeErr
			}
			client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
			open := insertSessionOpen(t, server, session, false, 2)
			body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}})
			if err != nil {
				t.Fatal(err)
			}
			message := wire.Message{Header: wire.Header{Command: wire.Close, SessionID: session.SessionID, TreeID: session.TreeID, MessageID: session.NextMessageID, CreditCharge: 1}, Body: body}
			response := exchange(ctx, t, client, message)[0]
			if response.Header.Status != smb.StatusFromError(closeErr) || storage.closed.Load() != 1 {
				t.Fatalf("cleanup: %+v, closed %d", response.Header, storage.closed.Load())
			}
			if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
				t.Fatal("closed open is still in the state table")
			}
		})
	}
}

func TestRequestBinding(t *testing.T) {
	request := RequestContext{Session: Session{SessionID: 17}, Tree: Tree{TreeID: 3}}
	if got := request.Binding(); got != (state.Binding{SessionID: 17, TreeID: 3}) {
		t.Fatalf("binding: %+v", got)
	}
}
