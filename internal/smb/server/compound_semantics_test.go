package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSignedFirstRelatedRefusesOnlyItsChain(t *testing.T) {
	for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
		t.Run(fmt.Sprintf("signing_%d", signing), func(t *testing.T) {
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
			first.Header.Flags |= wire.FlagRelated
			first.Header.TreeID = session.TreeID
			second := sessionEcho(t, session, session.NextMessageID+1)
			second.Header.Flags = wire.FlagRelated
			second.Header.SessionID, second.Header.TreeID = ^uint64(0), ^uint32(0)
			last := sessionEcho(t, session, session.NextMessageID+2)
			last.Header.TreeID = session.TreeID
			responses := exchange(ctx, t, client, first, second, last)
			if len(responses) != 3 {
				t.Fatalf("reply count: %d", len(responses))
			}
			for index, want := range []smb.Status{smb.StatusInvalidParameter, smb.StatusInvalidParameter, smb.StatusSuccess} {
				header := responses[index].Header
				if header.Status != want || header.SessionID != session.SessionID || header.TreeID != session.TreeID || header.MessageID != session.NextMessageID+uint64(index) || header.Flags&wire.FlagSigned == 0 || header.Credit == 0 {
					t.Errorf("member %d: %+v, want status %#x with original signed identity", index, header, want)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("dispatched %d ECHOs, want only the unrelated member", calls.Load())
			}
			response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+3))[0]
			if response.Header.Status != smb.StatusSuccess {
				t.Fatal("semantic refusal closed the connection")
			}
		})
	}
}

func TestFirstRelatedStillChecksProtectionBodiesAndCharges(t *testing.T) {
	for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
		for _, invalid := range []string{"unsigned", "body", "charge"} {
			t.Run(fmt.Sprintf("signing_%d_%s", signing, invalid), func(t *testing.T) {
				checkFirstRelatedValidation(t, signing, invalid)
			})
		}
	}
}

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

func TestEncryptedCompoundStillRejectsInvalidIdentity(t *testing.T) {
	for _, cipher := range []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, invalid := range []string{"first related", "unrelated session"} {
			t.Run(fmt.Sprintf("cipher_%d_%s", cipher, invalid), func(t *testing.T) {
				server, err := New(testOptions(t))
				if err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
					calls.Add(1)
					return handleEcho(ctx, request, message)
				}
				client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
				requests := []wire.Message{sessionEcho(t, session, session.NextMessageID)}
				if invalid == "first related" {
					requests[0].Header.Flags = wire.FlagRelated
				} else {
					second := sessionEcho(t, session, session.NextMessageID+1)
					second.Header.SessionID = ^uint64(0)
					requests = append(requests, second)
				}
				if err := client.Send(ctx, requests); err != nil {
					t.Fatal(err)
				}
				if response, err := client.ReceiveRaw(ctx); !errors.Is(err, io.EOF) || len(response) != 0 || calls.Load() != 0 {
					t.Fatalf("invalid encrypted identity: %x, %v, %d handlers", response, err, calls.Load())
				}
				healthy, healthyCtx, fresh := loginClient(t, server, cipher, smb.SigningGMAC)
				if response := exchange(healthyCtx, t, healthy, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
					t.Fatal("invalid identity stopped another connection")
				}
			})
		}
	}
}

func TestRelatedIdentityErrorWithoutFileIDKeepsPredecessorStatus(t *testing.T) {
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, sessionID, protector := rawLogin(t, server)
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: placeholderFileID()})
	if err != nil {
		t.Fatal(err)
	}
	first := wire.Message{Header: wire.Header{Command: wire.Close, MessageID: 3, SessionID: ^uint64(0), TreeID: ^uint32(0), CreditCharge: 1, Credit: 1}, Body: body}
	second := first
	second.Header.MessageID, second.Header.Flags = 4, wire.FlagRelated
	sendPayload(ctx, t, client, signMessages(t, protector, first, second))
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 2 {
		t.Fatalf("reply count: %d", len(response.Messages))
	}
	for _, message := range response.Messages {
		if message.Header.Status != smb.StatusUserSessionDeleted || message.Header.SessionID != ^uint64(0) {
			t.Fatalf("related identity error changed predecessor status: %+v", message.Header)
		}
	}
	request := echo(t, 5)
	request.Header.SessionID = sessionID
	sendPayload(ctx, t, client, signMessages(t, protector, request))
	if response := receiveSignedEcho(ctx, t, client, protector); response.Header.Status != smb.StatusSuccess {
		t.Fatal("missing session stopped the authenticated session")
	}
}
