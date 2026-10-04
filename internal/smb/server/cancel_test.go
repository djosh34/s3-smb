package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func cancelMessage(t *testing.T, target wire.Header, asynchronous bool) wire.Message {
	t.Helper()
	body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	header := wire.Header{Command: wire.Cancel, MessageID: target.MessageID, SessionID: target.SessionID}
	if asynchronous {
		header.Flags, header.AsyncID = wire.FlagAsync, target.AsyncID
		// An async CANCEL must use AsyncId, not this unrelated MessageId.
		header.MessageID = ^uint64(0)
	}
	return wire.Message{Header: header, Body: body}
}

func assertAsyncFinal(t *testing.T, final, pending wire.Message, status smb.Status) {
	t.Helper()
	header, saved := final.Header, pending.Header
	if header.Status != status || header.MessageID != saved.MessageID || header.SessionID != saved.SessionID || header.AsyncID != saved.AsyncID || header.Flags&wire.FlagAsync == 0 || header.Credit != 0 || header.Command != saved.Command {
		t.Fatalf("final identity/status/credits: %+v, pending: %+v", header, saved)
	}
	if status == smb.StatusCancelled {
		if _, err := wire.DecodeErrorResponse(final); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCancelByMessageIDAndAsyncID(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		for _, compound := range []bool{false, true} {
			t.Run(fmt.Sprintf("async_%t_compound_%t", asynchronous, compound), func(t *testing.T) {
				checkCancelIdentity(t, asynchronous, compound)
			})
		}
	}
}

func checkCancelIdentity(t *testing.T, asynchronous, compound bool) {
	t.Helper()
	server, _ := controlledAsync(t, wire.Read, reply{}, nil)
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
	cancel := cancelMessage(t, pending.Header, asynchronous)
	// CANCEL neither consumes this credit nor validates its charge.
	cancel.Header.CreditCharge, cancel.Header.Credit = ^uint16(0), ^uint16(0)
	if compound {
		cancel.Header.Flags |= wire.FlagRelated
		cancel.Header.SessionID = ^uint64(0)
		prefix := echo(t, 2)
		prefix.Header.SessionID = 77
		if err := client.Send(ctx, []wire.Message{prefix, cancel}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := client.Send(ctx, []wire.Message{cancel}); err != nil {
			t.Fatal(err)
		}
		if err := client.Send(ctx, []wire.Message{echo(t, 2)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[uint64]bool)
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatalf("CANCEL replied in compound: %+v", response.Messages)
		}
		message := response.Messages[0]
		if seen[message.Header.MessageID] {
			t.Fatal("duplicate final reply")
		}
		seen[message.Header.MessageID] = true
		switch message.Header.MessageID {
		case 1:
			assertAsyncFinal(t, message, pending, smb.StatusCancelled)
		case 2:
			if message.Header.Command != wire.Echo || message.Header.Status != smb.StatusSuccess {
				t.Fatal(message.Header)
			}
		default:
			t.Fatalf("CANCEL got a reply: %+v", message.Header)
		}
	}
	if response := exchange(ctx, t, client, echo(t, 3))[0]; response.Header.Command != wire.Echo || response.Header.MessageID != 3 {
		t.Fatal("extra completion")
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
