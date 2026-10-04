package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSessionSetupInCompoundIsRefusedBeforeDispatch(t *testing.T) {
	for _, setupFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("setup_first_%t", setupFirst), func(t *testing.T) {
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
			exchange(ctx, t, client, negotiateMessage(t, 16))
			body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{})
			if err != nil {
				t.Fatal(err)
			}
			setup := wire.Message{Header: wire.Header{Command: wire.SessionSetup, CreditCharge: 1, Credit: 16}, Body: body}
			messages := []wire.Message{echo(t, 1), setup}
			if setupFirst {
				messages[0], messages[1] = messages[1], messages[0]
			}
			messages[0].Header.MessageID, messages[1].Header.MessageID = 1, 2
			response := exchange(ctx, t, client, messages...)
			if len(response) != 2 || calls.Load() != 0 {
				t.Fatal("compound dispatched a handler")
			}
			for _, member := range response {
				if member.Header.Status != smb.StatusInvalidParameter {
					t.Fatal(member.Header)
				}
			}
			if response := exchange(ctx, t, client, echo(t, 3))[0]; response.Header.Status != smb.StatusSuccess {
				t.Fatal("compound refusal closed the connection")
			}
		})
	}
}
