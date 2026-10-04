package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Class-specific storage behaviour is tested with the real adapter separately.
func TestPendingSetInfoAllowsEcho(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		for _, test := range []struct {
			err    error
			name   string
			status smb.Status
		}{
			{name: "success", status: smb.StatusSuccess},
			{name: "storage_error", err: fmt.Errorf("controlled SET_INFO: %w", smb.ErrIO), status: smb.StatusIODeviceError},
		} {
			t.Run(fmt.Sprintf("%s/cipher_%d", test.name, cipher), func(t *testing.T) {
				server, release := controlledAsync(t, wire.SetInfo, setInfoTestReply(t), test.err)
				if cipher == 0 {
					server.options.Encryption = AllowPlaintext
				}
				client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
				request := setInfoAsyncMessage(t, session.NextMessageID)
				request.Header.SessionID, request.Header.TreeID = session.SessionID, session.TreeID
				if err := client.Send(ctx, []wire.Message{request}); err != nil {
					t.Fatal(err)
				}
				pending := receiveSetInfoReply(ctx, t, client)
				assertSetInfoAsync(t, request, pending, smb.StatusPending, pending.Header.AsyncID, 16)
				assertSetInfoEcho(ctx, t, client, session, request.Header.MessageID+1)
				close(release)
				final := receiveSetInfoReply(ctx, t, client)
				assertSetInfoAsync(t, request, final, test.status, pending.Header.AsyncID, 0)
				if test.err == nil {
					if _, err := wire.DecodeSetInfoResponse(final); err != nil {
						t.Fatal(err)
					}
				} else if _, err := wire.DecodeErrorResponse(final); err != nil {
					t.Fatal(err)
				}
				// A second interim or final would arrive instead of this ECHO.
				assertSetInfoEcho(ctx, t, client, session, request.Header.MessageID+2)
			})
		}
	}
}

func TestLocalSetInfoGetsOneSynchronousReply(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	result := setInfoTestReply(t)
	server.handlers[wire.SetInfo] = func(context.Context, RequestContext, wire.Message) (reply, error) {
		return result, nil
	}
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	request := setInfoAsyncMessage(t, 1)
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	response := receiveSetInfoReply(ctx, t, client)
	header := response.Header
	if header.Command != wire.SetInfo || header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.TreeID != request.Header.TreeID || header.Credit != 16 || header.CreditCharge != 1 {
		t.Fatalf("local SET_INFO identity/credits: %+v", header)
	}
	if _, err := wire.DecodeSetInfoResponse(response); err != nil {
		t.Fatal(err)
	}
	assertSetInfoEcho(ctx, t, client, smbtest.Session{SessionID: 77}, 2)
}

func TestCancelAffectsOnlyItsPendingSetInfo(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", async), func(t *testing.T) {
			server, release := controlledAsync(t, wire.SetInfo, setInfoTestReply(t), nil)
			client, ctx := corePipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 2))
			first, second := setInfoAsyncMessage(t, 1), setInfoAsyncMessage(t, 2)
			if err := client.Send(ctx, []wire.Message{first}); err != nil {
				t.Fatal(err)
			}
			firstPending := receiveSetInfoReply(ctx, t, client)
			assertSetInfoAsync(t, first, firstPending, smb.StatusPending, firstPending.Header.AsyncID, 16)
			if err := client.Send(ctx, []wire.Message{second}); err != nil {
				t.Fatal(err)
			}
			secondPending := receiveSetInfoReply(ctx, t, client)
			assertSetInfoAsync(t, second, secondPending, smb.StatusPending, secondPending.Header.AsyncID, 16)
			if firstPending.Header.AsyncID == secondPending.Header.AsyncID {
				t.Fatal("pending SET_INFO requests share an async ID")
			}
			body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
			if err != nil {
				t.Fatal(err)
			}
			cancel := wire.Message{Header: wire.Header{Command: wire.Cancel, MessageID: first.Header.MessageID, SessionID: first.Header.SessionID}, Body: body}
			if async {
				cancel.Header.MessageID, cancel.Header.AsyncID = 0, firstPending.Header.AsyncID
				cancel.Header.Flags = wire.FlagAsync
			}
			if err := client.Send(ctx, []wire.Message{cancel}); err != nil {
				t.Fatal(err)
			}
			final := receiveSetInfoReply(ctx, t, client)
			assertSetInfoAsync(t, first, final, smb.StatusCancelled, firstPending.Header.AsyncID, 0)
			session := smbtest.Session{SessionID: 77}
			assertSetInfoEcho(ctx, t, client, session, 3)
			close(release)
			final = receiveSetInfoReply(ctx, t, client)
			assertSetInfoAsync(t, second, final, smb.StatusSuccess, secondPending.Header.AsyncID, 0)
			if _, err := wire.DecodeSetInfoResponse(final); err != nil {
				t.Fatal(err)
			}
			assertSetInfoEcho(ctx, t, client, session, 4)
		})
	}
}

func setInfoAsyncMessage(t *testing.T, id uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileEndOfFile), Input: make([]byte, 8)})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.SetInfo, MessageID: id, SessionID: 77, TreeID: 12, CreditCharge: 1, Credit: 16}, Body: body}
}

func setInfoTestReply(t *testing.T) reply {
	t.Helper()
	body, err := wire.EncodeSetInfoResponse(wire.EmptyResponse{})
	if err != nil {
		t.Fatal(err)
	}
	return reply{body: body}
}

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

func assertSetInfoAsync(t *testing.T, request, response wire.Message, status smb.Status, asyncID uint64, credits uint16) {
	t.Helper()
	header := response.Header
	if header.Command != wire.SetInfo || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.Status != status || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || header.AsyncID != asyncID || header.TreeID != 0 || header.Credit != credits || header.CreditCharge != request.Header.CreditCharge {
		t.Fatalf("SET_INFO async identity/credits: %+v", header)
	}
}

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
