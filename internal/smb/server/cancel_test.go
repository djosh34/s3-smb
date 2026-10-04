package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCancelByMessageIDAndAsyncID(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		for _, compound := range []bool{false, true} {
			t.Run(fmt.Sprintf("async_%t_compound_%t", asynchronous, compound), func(t *testing.T) {
				checkCancelIdentity(t, asynchronous, compound)
			})
		}
	}
}

func TestCancelUnknownAndFinishedRequestsHasNoEffect(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", asynchronous), func(t *testing.T) {
			server, release := controlledAsync(t, wire.Read, reply{status: smb.StatusFileLockConflict}, nil)
			client, ctx := corePipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 2))
			pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
			unknown := pending.Header
			unknown.MessageID, unknown.AsyncID = 2, pending.Header.AsyncID+1
			if err := client.Send(ctx, []wire.Message{cancelMessage(t, unknown, asynchronous)}); err != nil {
				t.Fatal(err)
			}
			if response := exchange(ctx, t, client, echo(t, 2))[0]; response.Header.Command != wire.Echo {
				t.Fatal("unknown CANCEL replied or cancelled work")
			}
			close(release)
			final, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertAsyncFinal(t, final.Messages[0], pending, smb.StatusFileLockConflict)
			if err := client.Send(ctx, []wire.Message{cancelMessage(t, pending.Header, asynchronous)}); err != nil {
				t.Fatal(err)
			}
			if response := exchange(ctx, t, client, echo(t, 3))[0]; response.Header.Command != wire.Echo || response.Header.MessageID != 3 {
				t.Fatal("finished CANCEL replied")
			}
		})
	}
}

func TestCancelCannotCrossSessionOrConnection(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		for _, otherConnection := range []bool{false, true} {
			t.Run(fmt.Sprintf("async_%t_other_connection_%t", asynchronous, otherConnection), func(t *testing.T) {
				server, release := controlledAsync(t, wire.Read, reply{status: smb.StatusFileLockConflict}, nil)
				client, ctx := corePipeClient(t, server)
				exchange(ctx, t, client, negotiateMessage(t, 2))
				pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
				cancelClient := client
				cancel := cancelMessage(t, pending.Header, asynchronous)
				if otherConnection {
					cancelClient, _ = corePipeClient(t, server)
					exchange(ctx, t, cancelClient, negotiateMessage(t, 2))
					// Both connections have the same test session identity, MessageId
					// and AsyncId. Only the receiving connection may be affected.
					exchange(ctx, t, cancelClient, asyncMessage(t, wire.Read, 1))
				} else {
					cancel.Header.SessionID++
				}
				if err := cancelClient.Send(ctx, []wire.Message{cancel}); err != nil {
					t.Fatal(err)
				}
				if otherConnection {
					final, err := cancelClient.Receive(ctx)
					if err != nil {
						t.Fatal(err)
					}
					assertAsyncFinal(t, final.Messages[0], pending, smb.StatusCancelled)
				}
				if response := exchange(ctx, t, cancelClient, echo(t, 2))[0]; response.Header.Command != wire.Echo {
					t.Fatal("CANCEL crossed a session")
				}
				close(release)
				final, err := client.Receive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				assertAsyncFinal(t, final.Messages[0], pending, smb.StatusFileLockConflict)
			})
		}
	}
}

func TestCancelCannotTargetAnotherAuthenticatedSession(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", asynchronous), func(t *testing.T) {
			server, release := controlledAsync(t, wire.Read, reply{status: smb.StatusFileLockConflict}, nil)
			client := newRawSessions(t, server)
			first := client.start(t, 0)
			firstKey := client.finish(t, first, 0)
			second := client.start(t, 0)
			secondKey := client.finish(t, second, 0)
			body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\server\\backup"})
			if err != nil {
				t.Fatal(err)
			}
			tree := client.protected(t, firstKey, wire.Message{Header: wire.Header{Command: wire.TreeConnect, SessionID: first.id, CreditCharge: 1, Credit: 16}, Body: body})
			request := asyncMessage(t, wire.Read, client.nextID)
			request.Header.SessionID, request.Header.TreeID = first.id, tree.Header.TreeID
			pending := client.protected(t, firstKey, request)
			cancel := cancelMessage(t, pending.Header, asynchronous)
			cancel.Header.SessionID = second.id
			payload, err := wire.Join([]wire.Message{cancel})
			if err != nil {
				t.Fatal(err)
			}
			payload, err = secondKey.Seal(payload)
			if err != nil {
				t.Fatal(err)
			}
			sendPayload(client.ctx, t, client.client, payload)
			barrier := echo(t, client.nextID)
			barrier.Header.SessionID = second.id
			if response := client.protected(t, secondKey, barrier); response.Header.Command != wire.Echo || response.Header.Status != smb.StatusSuccess {
				t.Fatal("CANCEL crossed authenticated sessions or got a reply")
			}
			close(release)
			payload, err = client.client.ReceiveRaw(client.ctx)
			if err != nil {
				t.Fatal(err)
			}
			payload, err = firstKey.Open(payload)
			if err != nil {
				t.Fatal(err)
			}
			final, err := wire.Split(payload)
			if err != nil {
				t.Fatal(err)
			}
			assertAsyncFinal(t, final[0], pending, smb.StatusFileLockConflict)
		})
	}
}

func TestCancelHandlerThatFinishesAnywayRepliesOnce(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		// Cancellation lost the race with completed work. Its result wins.
		body, encodeErr := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("finished")})
		return reply{body: body}, encodeErr
	}
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
	if sendErr := client.Send(ctx, []wire.Message{cancelMessage(t, pending.Header, true)}); sendErr != nil {
		t.Fatal(sendErr)
	}
	final, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertAsyncFinal(t, final.Messages[0], pending, smb.StatusSuccess)
	body, err := wire.DecodeReadResponse(final.Messages[0])
	if err != nil || string(body.Data) != "finished" {
		t.Fatalf("handler result lost: %+v, %v", body, err)
	}
	if response := exchange(ctx, t, client, echo(t, 2))[0]; response.Header.Command != wire.Echo || response.Header.MessageID != 2 {
		t.Fatal("second final reply")
	}
}
