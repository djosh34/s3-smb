package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestTreeDisconnectAndLogoffCloseOnlyOwnedOpens(t *testing.T) {
	for _, command := range []wire.Command{wire.TreeDisconnect, wire.Logoff} {
		for _, cipher := range []uint16{0, smb.CipherAES128GCM} {
			t.Run(fmt.Sprintf("command_%d_cipher_%d", command, cipher), func(t *testing.T) { checkSessionCleanup(t, command, cipher) })
		}
	}
}

func TestTreeDisconnectLeavesAnotherTreeInTheSession(t *testing.T) {
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	other := session
	response := exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, wire.TreeConnect))[0]
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	other.TreeID = response.Header.TreeID
	open := insertSessionOpen(t, server, other, true, 5)
	exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID+1, wire.TreeDisconnect))
	if _, status := options.State.Find(open.ID, open.Binding); status != smb.StatusSuccess {
		t.Fatal("disconnect closed another tree")
	}
	exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID+2, wire.Logoff))
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("logoff left another tree's open")
	}
}

func TestSessionCleanupReturnsStorageError(t *testing.T) {
	for _, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		options := testOptions(t)
		storage := &cleanupStorage{closeErr: smb.ErrIO}
		options.Storage = storage
		server, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
		open := insertSessionOpen(t, server, session, false, 7)
		response := exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, command))[0]
		if response.Header.Status != smb.StatusIODeviceError {
			t.Fatalf("cleanup error lost: %+v", response.Header)
		}
		if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
			t.Fatal("cleanup error restored the open")
		}
	}
}

func TestTransportDropDetachesDurableAndClosesOrdinaryOpens(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	ordinary := insertSessionOpen(t, server, session, false, 8)
	durable := insertSessionOpen(t, server, session, true, 9)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	// Wait for connection teardown, not server shutdown, which closes durability.
	server.workers.Wait()
	if storage.closed.Load() != 1 {
		t.Fatalf("drop closed %d opens", storage.closed.Load())
	}
	if _, status := options.State.Find(ordinary.ID, ordinary.Binding); status == smb.StatusSuccess {
		t.Fatal("ordinary open survived drop")
	}
	if _, status := options.State.Find(durable.ID, durable.Binding); status == smb.StatusSuccess {
		t.Fatal("durable open remains attached")
	}
	attached, status := options.State.Reconnect(state.ReconnectRequest{ID: durable.ID, Binding: state.Binding{SessionID: 99, TreeID: 99}, User: durable.User, Share: durable.Share, ClientGUID: durable.ClientGUID, CreateGUID: durable.CreateGUID, LeaseKey: durable.LeaseKey})
	if status != smb.StatusSuccess || attached.ID.Volatile == durable.ID.Volatile {
		t.Fatalf("durability lost: %+v %#x", attached, status)
	}
	if err := server.Shutdown(context.WithoutCancel(ctx)); err != nil {
		t.Fatal(err)
	}
}

func TestLogoffCancelsAndDrainsPendingWork(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		close(drained)
		return reply{}, ctx.Err()
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	insertSessionOpen(t, server, session, false, 10)
	pending := exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, wire.Read))[0]
	if pending.Header.Status != smb.StatusPending {
		t.Fatal(pending.Header)
	}
	if err := client.Send(ctx, []wire.Message{treeRequest(t, session, session.NextMessageID+1, wire.Logoff)}); err != nil {
		t.Fatal(err)
	}
	seen := map[wire.Command]bool{}
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		if seen[header.Command] {
			t.Fatal("duplicate completion")
		}
		seen[header.Command] = true
		if header.Command == wire.Logoff {
			select {
			case <-drained:
			default:
				t.Fatal("logoff did not drain work")
			}
			if header.Status != smb.StatusSuccess || storage.closed.Load() != 1 {
				t.Fatal("logoff did not clean up")
			}
		} else if header.Command != wire.Read || header.Status != smb.StatusCancelled || header.Credit != 0 {
			t.Fatal(header)
		}
	}
	if !seen[wire.Read] || !seen[wire.Logoff] {
		t.Fatal("missing completion")
	}
}

func TestLogoffCleanupErrorIsLogged(t *testing.T) {
	options := testOptions(t)
	logs := &recordedHandler{}
	options.Logger = slog.New(logs)
	options.Storage = &cleanupStorage{closeErr: errors.New("controlled close failure")}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	insertSessionOpen(t, server, session, false, 11)
	exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, wire.Logoff))
	entries := logs.snapshot()
	if len(entries) != 1 || entries[0].message != "request failed" {
		t.Fatalf("cleanup error not logged: %+v", entries)
	}
}
