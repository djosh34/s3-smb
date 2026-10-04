package server

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestNewRejectsInvalidOptions(t *testing.T) {
	for _, test := range []struct {
		invalidate func(*Options)
		name       string
	}{
		{name: "storage", invalidate: func(o *Options) { o.Storage = nil }},
		{name: "typed nil storage", invalidate: func(o *Options) { o.Storage = (*unusedStorage)(nil) }},
		{name: "state", invalidate: func(o *Options) { o.State = nil }},
		{name: "logger", invalidate: func(o *Options) { o.Logger = nil }},
		{name: "clock", invalidate: func(o *Options) { o.Now = nil }},
		{name: "account", invalidate: func(o *Options) { o.Account.User = "" }},
		{name: "server name", invalidate: func(o *Options) { o.ServerName = "" }},
		{name: "share name", invalidate: func(o *Options) { o.ShareName = "../backup" }},
		{name: "IPC share", invalidate: func(o *Options) { o.ShareName = "ipc$" }},
		{name: "GUID", invalidate: func(o *Options) { o.ServerGUID = [16]byte{} }},
		{name: "policy", invalidate: func(o *Options) { o.Encryption = 99 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(t)
			test.invalidate(&options)
			if server, err := New(options); err == nil || server != nil {
				t.Fatalf("invalid options accepted: %v", err)
			}
		})
	}
}

func TestServeConnCancellationClosesTransport(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServeConn cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled connection did not stop")
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestServeAndShutdownOwnListenerAndConnections(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
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
	exchange(ctx, t, client, negotiateMessage(t, 1))
	messages := exchange(ctx, t, client, echo(t, 1))
	if messages[0].Header.Status != smb.StatusSuccess {
		t.Fatal("listener did not serve ECHO")
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := client.Receive(ctx); err == nil {
		t.Fatal("shutdown left connection open")
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	if err := server.ServeConn(ctx, local); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("connection after shutdown: %v", err)
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
}

type cleanupHandle struct{ object smb.ObjectKey }

func (handle cleanupHandle) Key() smb.ObjectKey { return handle.object }

// This records shutdown calls, not filesystem coherence or file operations.
type cleanupStorage struct {
	smb.Storage
	closeErr  error
	removeErr error
	closed    atomic.Int32
	removed   atomic.Int32
}

func (storage *cleanupStorage) Close(context.Context, smb.Handle) error {
	storage.closed.Add(1)
	return storage.closeErr
}
func (*cleanupStorage) PathOf(context.Context, smb.Inode) (string, error) { return "renamed", nil }
func (*cleanupStorage) Lookup(context.Context, string) (smb.Resolved, error) {
	return smb.Resolved{Exists: true, Object: smb.ObjectKey{Inode: 2}, Name: smb.Name{Parent: 1, Base: "renamed"}}, nil
}

func (storage *cleanupStorage) Remove(_ context.Context, name smb.Name, inode smb.Inode) error {
	if name.Base != "renamed" || inode != 2 {
		return errors.New("shutdown used a stale deletion name")
	}
	storage.removed.Add(1)
	return storage.removeErr
}

func TestShutdownClosesOpensAppliesDeletionAndReturnsAllErrors(t *testing.T) {
	options := testOptions(t)
	closeErr, removeErr := errors.New("close failed"), errors.New("remove failed")
	storage := &cleanupStorage{closeErr: closeErr, removeErr: removeErr}
	options.Storage = storage
	object := smb.ObjectKey{Inode: 2}
	reservation, status := options.State.Reserve(state.OpenRequest{Object: object, Binding: state.Binding{SessionID: 1, TreeID: 1}, GrantedAccess: 0x10000, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	if _, status := options.State.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}, DeleteOnClose: true, DeleteName: smb.Name{Parent: 1, Base: "old"}}); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := server.Shutdown(t.Context())
		if !errors.Is(err, closeErr) || !errors.Is(err, removeErr) {
			t.Fatalf("shutdown errors: %v", err)
		}
	}
	if storage.closed.Load() != 1 || storage.removed.Load() != 1 {
		t.Fatalf("cleanup repeated: close %d remove %d", storage.closed.Load(), storage.removed.Load())
	}
}
