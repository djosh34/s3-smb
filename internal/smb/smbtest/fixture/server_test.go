package fixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func fixtureOptions(t *testing.T) server.Options {
	t.Helper()
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return server.Options{
		Storage: smbtest.NewStorage(t), State: table,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		Account:   auth.Account{User: "backup", Password: "password"},
		ShareName: "TimeMachine", ServerName: "s3-smb", ServerGUID: [16]byte{1},
	}
}

func closeContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
}

func TestStartWithRealAdapterAndRawLogin(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM, smb.CipherAES256GCM} {
		t.Run(fmt.Sprintf("cipher_%d", cipher), func(t *testing.T) { checkFixtureLogin(t, cipher) })
	}
}

func startTestFixture(t *testing.T, options server.Options) *Server {
	t.Helper()
	fixture, err := Start(context.WithoutCancel(t.Context()), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := closeContext(t)
		defer cancel()
		if closeErr := fixture.Close(ctx); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return fixture
}

func fixtureClient(ctx context.Context, t *testing.T, fixture *Server) *smbtest.Client {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", fixture.Address())
	if err != nil {
		t.Fatal(err)
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(errors.Join(err, conn.Close()))
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

func checkFixtureLogin(t *testing.T, cipher uint16) {
	t.Helper()
	options := fixtureOptions(t)
	if cipher == 0 {
		options.Encryption = server.AllowPlaintext
	}
	fixture := startTestFixture(t, options)
	host, port, err := net.SplitHostPort(fixture.Address())
	if err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("fixture address: %q, %v", fixture.Address(), err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := fixtureClient(ctx, t, fixture)
	session, err := client.Login(ctx, smbtest.LoginOptions{
		Account: options.Account, Share: options.ShareName,
		Cipher: cipher, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2},
	})
	if err != nil || session.SessionID == 0 || session.TreeID == 0 {
		t.Fatalf("raw login: %+v, %v", session, err)
	}
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := client.Send(ctx, []wire.Message{{Header: wire.Header{
		Command: wire.Echo, MessageID: session.NextMessageID,
		SessionID: session.SessionID, CreditCharge: 1, Credit: 1,
	}, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	reply, err := client.Receive(ctx)
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("protected echo: %+v, %v", reply, err)
	}
	if closeErr := fixture.Close(ctx); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, receiveErr := client.Receive(ctx); receiveErr == nil {
		t.Fatal("fixture left the client connection open")
	}
	root, err := options.Storage.Lookup(ctx, "")
	if err != nil || !root.Exists {
		t.Fatalf("fixture closed its storage runtime: %+v, %v", root, err)
	}
}

func TestStartRejectsInvalidInputs(t *testing.T) {
	if fixture, err := Start(t.Context(), server.Options{}); err == nil || fixture != nil {
		t.Fatalf("invalid options: %v, %v", fixture, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if fixture, err := Start(ctx, server.Options{}); !errors.Is(err, context.Canceled) || fixture != nil {
		t.Fatalf("canceled start: %v, %v", fixture, err)
	}
}

func TestCloseImmediatelyAndConcurrently(t *testing.T) {
	fixture, err := Start(context.WithoutCancel(t.Context()), fixtureOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := closeContext(t)
	defer cancel()
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if closeErr := fixture.Close(ctx); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	workers.Wait()
	select {
	case <-fixture.done:
	default:
		t.Fatal("Close did not wait for Serve")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", fixture.Address())
	if err == nil {
		t.Fatal(errors.Join(errors.New("fixture listener remained open"), conn.Close()))
	}
}

// Inject a cleanup failure around real storage, without replacing its semantics.
type cleanupStorage struct {
	smb.Storage
	err     error
	entered chan struct{}
	release chan struct{}
	closes  atomic.Int32
}

func (storage *cleanupStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.closes.Add(1)
	if storage.entered != nil {
		close(storage.entered)
		<-storage.release
	}
	return errors.Join(storage.Storage.Close(ctx, handle), storage.err)
}

func seedOpen(t *testing.T, options server.Options) {
	t.Helper()
	resolved, err := options.Storage.Lookup(t.Context(), "band")
	if err != nil {
		t.Fatal(err)
	}
	created, err := options.Storage.Create(t.Context(), resolved.Name, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := options.Storage.Open(t.Context(), created.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	reservation, status := options.State.Reserve(state.OpenRequest{
		Object: created.Object, Binding: state.Binding{SessionID: 1, TreeID: 1},
		User: options.Account.User, Share: options.ShareName,
	})
	if status != smb.StatusSuccess {
		t.Fatalf("reserve: %v; close: %v", status, options.Storage.Close(context.WithoutCancel(t.Context()), handle))
	}
	if _, status := options.State.Commit(reservation, state.Grant{Handle: handle}); status != smb.StatusSuccess {
		t.Fatalf("commit: %v; abort: %v; close: %v", status, options.State.Abort(reservation), options.Storage.Close(context.WithoutCancel(t.Context()), handle))
	}
}

func TestCloseReturnsShutdownErrors(t *testing.T) {
	options := fixtureOptions(t)
	failure := errors.New("injected storage close failure")
	storage := &cleanupStorage{Storage: options.Storage, err: failure}
	options.Storage = storage
	seedOpen(t, options)
	fixture, err := Start(context.WithoutCancel(t.Context()), options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := closeContext(t)
	defer cancel()
	for range 2 {
		if err := fixture.Close(ctx); !errors.Is(err, failure) {
			t.Fatalf("shutdown error lost: %v", err)
		}
	}
	if got := storage.closes.Load(); got != 1 {
		t.Fatalf("storage closed %d times, want 1", got)
	}
}

func TestCanceledCloseCanBeRetried(t *testing.T) {
	options := fixtureOptions(t)
	storage := &cleanupStorage{Storage: options.Storage, entered: make(chan struct{}), release: make(chan struct{})}
	options.Storage = storage
	seedOpen(t, options)
	fixture, err := Start(context.WithoutCancel(t.Context()), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(storage.release)
		ctx, cancel := closeContext(t)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixture.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled close: %v", err)
	}
	select {
	case <-storage.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not begin cleanup")
	}
}

func TestStartContextCancellationStopsServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	defer cancel()
	fixture, err := Start(ctx, fixtureOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	closeCtx, stop := closeContext(t)
	defer stop()
	if err := fixture.Close(closeCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start cancellation was not reported: %v", err)
	}
}
