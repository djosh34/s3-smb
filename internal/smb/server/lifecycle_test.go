package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestNewRejectsInvalidOptions(t *testing.T) {
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	valid := Options{
		Storage: &faultStorage{}, State: table, Logger: slog.New(slog.DiscardHandler), Now: time.Now,
		Account: auth.Account{User: "backup", Password: "password"}, ShareName: "backup", ServerName: "s3-smb", ServerGUID: [16]byte{1},
	}
	if _, err := New(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Options){
		"no storage":                func(options *Options) { options.Storage = nil },
		"no user":                   func(options *Options) { options.Account.User = "" },
		"IPC$ share":                func(options *Options) { options.ShareName = "ipc$" },
		"share name with a slash":   func(options *Options) { options.ShareName = "back/up" },
		"zero server GUID":          func(options *Options) { options.ServerGUID = [16]byte{} },
		"unknown encryption policy": func(options *Options) { options.Encryption = 2 },
	} {
		options := valid
		change(&options)
		if _, err := New(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Serve owns its listener: Shutdown closes it with every connection, and the
// server takes no connection after that.
func TestServeAndShutdown(t *testing.T) {
	srv := newTestServer(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.server.Serve(t.Context(), listener) }()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := client.Login(t.Context(), smbtest.LoginOptions{Share: "backup", Account: srv.server.options.Account, Signing: smb.SigningGMAC}); err != nil {
		t.Fatal(err)
	}
	if err := srv.server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("connection after shutdown: %v", err)
	}
	if err := srv.accept(t).ended(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("new connection after shutdown: %v", err)
	}
}

// Shutdown closes the opens of live connections, which report what storage
// failed to close, and then detached durable opens. It deletes what is to be
// deleted on close, also when the close fails, and reports the failure.
// Cleanup goes on after the caller gives up, and later calls wait for it.
func TestShutdownClosesEveryOpen(t *testing.T) {
	srv := newTestServer(t)
	dropped := srv.connect(t)
	mustCreate(t, dropped, durableCreate("kept", 1))
	dropped.drop(t)
	client := srv.connect(t)
	openAs(t, client, "doomed", fileAllAccess, 7, fileDeleteOnClose)
	failure := errors.New("close failed")
	srv.shutdownErr = failure
	entered, blocked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	t.Cleanup(release)
	var closes atomic.Int32
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Close = func(ctx context.Context, handle smb.Handle) error {
			if closes.Add(1) == 1 {
				return errors.Join(srv.adapter.Close(ctx, handle), failure)
			}
			close(entered)
			<-blocked
			return srv.adapter.Close(ctx, handle)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.server.Shutdown(ctx) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error %v", err)
	}
	release()
	for range 2 {
		if err := srv.server.Shutdown(t.Context()); !errors.Is(err, failure) {
			t.Fatalf("shutdown error %v, want %v", err, failure)
		}
	}
	if err := client.ended(); !errors.Is(err, failure) {
		t.Fatalf("connection error %v", err)
	}
	if closes.Load() != 2 {
		t.Fatalf("%d closes", closes.Load())
	}
	srv.expectContent(t, map[string]string{"doomed": ""})
}

// TREE_DISCONNECT closes the opens of its tree, LOGOFF those of every tree in
// the session.
func TestTreeDisconnectAndLogoff(t *testing.T) {
	srv := newTestServer(t)
	client, other := srv.connect(t), srv.connect(t)
	first := openAs(t, client, "first", fileAllAccess, 0, 0)
	firstTree := client.session.TreeID
	reply := client.call(t, wire.TreeConnect, encode(t, wire.EncodeTreeConnectRequest, wire.TreeConnectRequest{Path: `\\host\backup`}), 1)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("TREE_CONNECT status %#x", reply.Header.Status)
	}
	client.session.TreeID = reply.Header.TreeID
	openAs(t, client, "second", fileAllAccess, 0, 0)
	client.session.TreeID = firstTree
	empty := encode(t, wire.EncodeTreeDisconnectRequest, wire.EmptyRequest{})
	if status := client.call(t, wire.TreeDisconnect, empty, 1).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("TREE_DISCONNECT status %#x", status)
	}
	if status := client.close(t, first); status != smb.StatusNetworkNameDeleted {
		t.Fatalf("CLOSE on the disconnected tree: status %#x", status)
	}
	if openStatus(t, other, "first") != smb.StatusSuccess || openStatus(t, other, "second") != smb.StatusSharingViolation {
		t.Fatal("TREE_DISCONNECT closed the wrong opens")
	}
	client.session.TreeID = reply.Header.TreeID
	if status := client.call(t, wire.Logoff, empty, 1).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("LOGOFF status %#x", status)
	}
	if status := openStatus(t, other, "second"); status != smb.StatusSuccess {
		t.Fatalf("open after LOGOFF: status %#x", status)
	}
}
