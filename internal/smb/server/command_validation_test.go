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

func TestUnknownCommandClosesOnlyItsConnection(t *testing.T) {
	for _, command := range []wire.Command{0xff, 0xffff} {
		for _, compound := range []bool{false, true} {
			t.Run(fmt.Sprintf("command_%x_compound_%t", command, compound), func(t *testing.T) {
				server, err := New(testOptions(t))
				if err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
					calls.Add(1)
					return handleEcho(ctx, request, message)
				}
				client, ctx := pipeClient(t, server)
				healthy, healthyCtx := pipeClient(t, server)
				exchange(ctx, t, client, negotiateMessage(t, 4))
				exchange(healthyCtx, t, healthy, negotiateMessage(t, 4))
				unknown := echo(t, 1)
				unknown.Header.Command = command
				requests := []wire.Message{unknown}
				if compound {
					unknown.Header.MessageID = 2
					requests = []wire.Message{echo(t, 1), unknown}
				}
				if err := client.Send(ctx, requests); err != nil {
					t.Fatal(err)
				}
				if response, err := client.ReceiveRaw(ctx); !errors.Is(err, io.EOF) || len(response) != 0 {
					t.Fatalf("unknown command must disconnect without a response: %x, %v", response, err)
				}
				if calls.Load() != 0 {
					t.Fatal("unknown-command compound dispatched a prefix handler")
				}
				response := exchange(healthyCtx, t, healthy, echo(t, 1))[0]
				if response.Header.Status != smb.StatusSuccess || calls.Load() != 1 {
					t.Fatal("invalid command affected another connection")
				}
			})
		}
	}
}

func TestValidUnsupportedCommandStillReturnsStatus(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(fmt.Sprintf("cipher_%d", cipher), func(t *testing.T) {
			options := testOptions(t)
			if cipher == 0 {
				options.Encryption = AllowPlaintext
			}
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
			body, err := wire.EncodeChangeNotifyRequest(wire.ChangeNotifyRequest{})
			if err != nil {
				t.Fatal(err)
			}
			request := wire.Message{Header: wire.Header{Command: wire.ChangeNotify, MessageID: session.NextMessageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
			if response := exchange(ctx, t, client, request)[0]; response.Header.Status != smb.StatusNotSupported {
				t.Fatalf("valid unsupported command: %+v", response.Header)
			}
			if response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+1))[0]; response.Header.Status != smb.StatusSuccess {
				t.Fatal("valid unsupported command closed the connection")
			}
		})
	}
}
