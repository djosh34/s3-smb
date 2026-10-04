package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type drainCleanupStorage struct {
	*cleanupStorage
	drained <-chan struct{}
}

func (storage *drainCleanupStorage) Close(ctx context.Context, handle smb.Handle) error {
	select {
	case <-storage.drained:
		return storage.cleanupStorage.Close(ctx, handle)
	default:
		return errors.New("cleanup ran before the compound handler drained")
	}
}

func checkCompoundLogoff(ctx context.Context, t *testing.T, client *smbtest.Client, pending map[uint64]wire.Message) {
	t.Helper()
	seenLogoff := false
	count := len(pending) + 1
	for range count {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		message := response.Messages[0]
		if message.Header.Command == wire.Logoff {
			if seenLogoff || message.Header.Status != smb.StatusSuccess {
				t.Fatal(message.Header)
			}
			seenLogoff = true
			continue
		}
		saved, exists := pending[message.Header.MessageID]
		if !exists {
			t.Fatalf("unexpected or duplicate completion: %+v", message.Header)
		}
		assertAsyncFinal(t, message, saved, smb.StatusCancelled)
		delete(pending, message.Header.MessageID)
	}
	if !seenLogoff || len(pending) != 0 {
		t.Fatal("missing logoff or related completion")
	}
}

func waitCompoundSignal(ctx context.Context, t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(message)
	}
}

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

func checkCompoundDrain(t *testing.T, mode, stage string) {
	t.Helper()
	options := testOptions(t)
	prefixDrained, memberDrained := make(chan struct{}), make(chan struct{})
	storage := &drainCleanupStorage{cleanupStorage: &cleanupStorage{}, drained: prefixDrained}
	if stage == "running" {
		storage.drained = memberDrained
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release, entered := make(chan struct{}), make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		defer close(prefixDrained)
		select {
		case <-release:
			body, encodeErr := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			return reply{body: body}, encodeErr
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	server.handlers[wire.Flush] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		close(entered)
		<-ctx.Done()
		close(memberDrained)
		return reply{}, ctx.Err()
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	open := insertSessionOpen(t, server, session, false, 40)
	read := treeRequest(t, session, session.NextMessageID, wire.Read)
	member := asyncMessage(t, wire.Flush, session.NextMessageID+1)
	member.Header.Flags = wire.FlagRelated
	member.Header.SessionID, member.Header.TreeID = ^uint64(0), ^uint32(0)
	if sendErr := client.Send(ctx, []wire.Message{read, member}); sendErr != nil {
		t.Fatal(sendErr)
	}
	pending := make(map[uint64]wire.Message)
	for range 2 {
		response, receiveErr := client.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		message := response.Messages[0]
		if message.Header.Status != smb.StatusPending || message.Header.SessionID != session.SessionID {
			t.Fatal(message.Header)
		}
		pending[message.Header.MessageID] = message
	}
	if stage == "running" {
		close(release)
		waitCompoundSignal(ctx, t, entered, "related member never started")
		response, receiveErr := client.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		assertAsyncFinal(t, response.Messages[0], pending[read.Header.MessageID], smb.StatusSuccess)
		delete(pending, read.Header.MessageID)
	}
	switch mode {
	case "disconnect":
		if closeErr := client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		done := make(chan struct{})
		go func() { server.workers.Wait(); close(done) }()
		waitCompoundSignal(ctx, t, done, "connection cleanup did not finish")
	case "shutdown":
		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			t.Fatal("shutdown did not drain related work", shutdownErr)
		}
	case "logoff":
		if sendErr := client.Send(ctx, []wire.Message{treeRequest(t, session, session.NextMessageID+2, wire.Logoff)}); sendErr != nil {
			t.Fatal(sendErr)
		}
		checkCompoundLogoff(ctx, t, client, pending)
	}
	select {
	case <-storage.drained:
	default:
		t.Fatal("handler did not drain")
	}
	if storage.closed.Load() != 1 {
		t.Fatalf("cleanup closed %d opens, want 1", storage.closed.Load())
	}
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("cleanup left the open attached")
	}
	if stage == "waiting" {
		select {
		case <-entered:
			t.Fatal("cancelled dependent handler started")
		default:
		}
	}
}
