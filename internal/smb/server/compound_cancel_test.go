package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCancelWaitingRelatedMemberDoesNotCancelPrerequisite(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		t.Run(map[bool]string{false: "message_id", true: "async_id"}[asynchronous], func(t *testing.T) {
			body, err := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			if err != nil {
				t.Fatal(err)
			}
			server, release := controlledAsync(t, wire.Read, reply{body: body}, nil)
			entered := make(chan struct{})
			server.handlers[wire.Flush] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
				close(entered)
				<-ctx.Done()
				return reply{}, ctx.Err()
			}
			client, ctx := corePipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 3))
			member := asyncMessage(t, wire.Flush, 2)
			member.Header.Flags = wire.FlagRelated
			member.Header.SessionID, member.Header.TreeID = ^uint64(0), ^uint32(0)
			if sendErr := client.Send(ctx, []wire.Message{asyncMessage(t, wire.Read, 1), member}); sendErr != nil {
				t.Fatal(sendErr)
			}
			var pending []wire.Message
			for range 2 {
				response, receiveErr := client.Receive(ctx)
				if receiveErr != nil {
					t.Fatal(receiveErr)
				}
				pending = append(pending, response.Messages[0])
			}
			if sendErr := client.Send(ctx, []wire.Message{cancelMessage(t, pending[1].Header, asynchronous)}); sendErr != nil {
				t.Fatal(sendErr)
			}
			final, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertAsyncFinal(t, final.Messages[0], pending[1], smb.StatusCancelled)
			close(release)
			final, err = client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertAsyncFinal(t, final.Messages[0], pending[0], smb.StatusSuccess)
			select {
			case <-entered:
				t.Fatal("cancelled dependent handler ran")
			default:
			}
			if response := exchange(ctx, t, client, echo(t, 3))[0]; response.Header.Command != wire.Echo || response.Header.MessageID != 3 {
				t.Fatal("extra cancellation reply")
			}
		})
	}
}

// Regression for #100: related work must not keep cleanup or shutdown waiting.
func TestRelatedCompoundCancellationDrainsBeforeCleanup(t *testing.T) {
	for _, mode := range []string{"disconnect", "logoff", "shutdown"} {
		for _, stage := range []string{"waiting", "running"} {
			t.Run(mode+"_"+stage, func(t *testing.T) { checkCompoundDrain(t, mode, stage) })
		}
	}
}
