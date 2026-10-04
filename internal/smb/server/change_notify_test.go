package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Regression for #107: neither request can replace the other's identity or own
// pending work, even if someone registers a CHANGE_NOTIFY handler.
func TestChangeNotifySameDirectoryHandle(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(notifyProtectionName(cipher), func(t *testing.T) {
			server := notifyServer(t, cipher)
			var calls atomic.Int32
			server.handlers[wire.ChangeNotify] = func(context.Context, RequestContext, wire.Message) (reply, error) {
				calls.Add(1)
				return reply{status: smb.StatusSuccess}, nil
			}
			if asyncEligible(wire.ChangeNotify) {
				t.Fatal("CHANGE_NOTIFY is async-eligible")
			}
			client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
			open := insertNotifyDirectory(t, server, session)
			requests := []wire.Message{notifyMessage(t, open, session.NextMessageID), notifyMessage(t, open, session.NextMessageID+1)}
			requests[0].Header.Credit, requests[1].Header.Credit = 0, 7
			sent := make(chan error, 1)
			go func() {
				for _, request := range requests {
					if err := client.Send(ctx, []wire.Message{request}); err != nil {
						sent <- err
						return
					}
				}
				sent <- nil
			}()
			for _, request := range requests {
				response, err := client.Receive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(response.Messages) != 1 {
					t.Fatalf("CHANGE_NOTIFY reply members: %d", len(response.Messages))
				}
				checkNotifyRefusal(t, response.Messages[0], request, smb.StatusNotSupported)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			checkFollowingEcho(ctx, t, client, session, session.NextMessageID+2)
			checkNotifyState(t, server, open)
			if calls.Load() != 0 {
				t.Fatal("CHANGE_NOTIFY ran a registered handler")
			}
		})
	}
}

// Regression for #146: both compound orders get one complete frame. No pending
// reply or delayed completion can resend the earlier compound before ECHO.
func TestChangeNotifyCompoundDoesNotResend(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(notifyProtectionName(cipher), func(t *testing.T) {
			for _, notifyFirst := range []bool{false, true} {
				name := "echo_first"
				if notifyFirst {
					name = "notify_first"
				}
				t.Run(name, func(t *testing.T) { checkNotifyCompound(t, cipher, notifyFirst) })
			}
		})
	}
}

func TestChangeNotifyChecksSessionAndTree(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(notifyProtectionName(cipher), func(t *testing.T) {
			server := notifyServer(t, cipher)
			var calls atomic.Int32
			server.handlers[wire.ChangeNotify] = func(context.Context, RequestContext, wire.Message) (reply, error) {
				calls.Add(1)
				return reply{status: smb.StatusSuccess}, nil
			}
			client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
			_, _, foreign := loginClient(t, server, cipher, smb.SigningGMAC)
			open := insertNotifyDirectory(t, server, session)
			for index, status := range []smb.Status{smb.StatusNetworkNameDeleted, smb.StatusNotSupported} {
				request := notifyMessage(t, open, session.NextMessageID+uint64(index))
				if index == 0 {
					request.Header.TreeID = foreign.TreeID
				}
				responses := exchange(ctx, t, client, request)
				if len(responses) != 1 {
					t.Fatalf("CHANGE_NOTIFY reply members: %d", len(responses))
				}
				checkNotifyRefusal(t, responses[0], request, status)
			}
			checkFollowingEcho(ctx, t, client, session, session.NextMessageID+2)
			// A foreign session cannot authenticate a transform on this connection.
			// Check its dispatch status on a separate negotiated transport.
			plain, plainCtx := pipeClient(t, server)
			exchange(plainCtx, t, plain, negotiateMessage(t, 1))
			request := notifyMessage(t, open, 1)
			request.Header.SessionID = foreign.SessionID
			responses := exchange(plainCtx, t, plain, request)
			if len(responses) != 1 {
				t.Fatalf("missing session reply members: %d", len(responses))
			}
			checkNotifyRefusal(t, responses[0], request, smb.StatusUserSessionDeleted)
			checkFollowingEcho(plainCtx, t, plain, smbtest.Session{}, 2)
			checkNotifyState(t, server, open)
			if calls.Load() != 0 {
				t.Fatal("CHANGE_NOTIFY ran a registered handler")
			}
		})
	}
}

func TestRelatedChangeNotifyFollowsPendingPredecessor(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(notifyProtectionName(cipher), func(t *testing.T) {
			for _, test := range []struct {
				name   string
				status smb.Status
			}{
				{"success", smb.StatusSuccess},
				{"warning", smb.StatusBufferOverflow},
				{"error", smb.StatusIODeviceError},
			} {
				t.Run(test.name, func(t *testing.T) { checkRelatedNotify(t, cipher, test.status) })
			}
		})
	}
}
