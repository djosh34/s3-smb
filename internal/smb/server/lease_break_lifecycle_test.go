package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Done observes entry into the send or break wait, without advancing the clock.
type observedBreakWait struct {
	context.Context
	waiting chan struct{}
	calls   atomic.Int32
	after   int32
}

func (ctx *observedBreakWait) Done() <-chan struct{} {
	if ctx.calls.Add(1) == ctx.after {
		close(ctx.waiting)
	}
	return ctx.Context.Done()
}

func fixedClockLeaseServer(t *testing.T) (*Server, *cleanupStorage) {
	t.Helper()
	options := testOptions(t)
	now := time.Now()
	options.Now = func() time.Time { return now }
	var err error
	options.State, err = state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server, storage
}

func TestEarlierLeaseBreakCompletesAfterOriginalWaiterCancels(t *testing.T) {
	server, storage := fixedClockLeaseServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
	firstCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := startServerBreak(firstCtx, server, open, smb.LeaseRead|smb.LeaseHandle)
	if _, err := client.WaitLeaseBreak(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waitAsyncResult(ctx, t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("original waiter: %v", err)
	}
	secondCtx := &observedBreakWait{Context: ctx, waiting: make(chan struct{}), after: 1}
	second := startServerBreak(secondCtx, server, open, smb.LeaseRead|smb.LeaseHandle)
	waitAsyncSignal(ctx, t, secondCtx.waiting)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	finishServerBreak(ctx, t, second)
	if storage.closed.Load() != 1 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("earlier detached break kept rights or its handle")
	}
}

func TestSharedLeaseBreakCompletesAfterLastAttachedMemberCloses(t *testing.T) {
	for _, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		t.Run(map[wire.Command]string{wire.Logoff: "logoff", wire.TreeDisconnect: "tree disconnect"}[command], func(t *testing.T) {
			server, storage := fixedClockLeaseServer(t)
			client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
			_, _, other := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
			open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
			member := joinDurableLease(t, server, other, open)
			if actions := server.options.State.Disconnect(other.SessionID); len(actions) != 0 {
				t.Fatal("durable member closed on disconnect")
			}
			waitCtx := &observedBreakWait{Context: ctx, waiting: make(chan struct{}), after: 2}
			done := startServerBreak(waitCtx, server, open, smb.LeaseRead|smb.LeaseHandle)
			if _, err := client.WaitLeaseBreak(ctx); err != nil {
				t.Fatal(err)
			}
			waitAsyncSignal(ctx, t, waitCtx.waiting)
			response := exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, command))[0]
			if response.Header.Status != smb.StatusSuccess {
				t.Fatal(response.Header)
			}
			finishServerBreak(ctx, t, done)
			if storage.closed.Load() != 2 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
				t.Fatal("shared detached break kept rights or a handle")
			}
			if _, status := server.options.State.Reconnect(state.ReconnectRequest{ID: member.ID, Binding: member.Binding, User: member.User, Share: member.Share, ClientGUID: member.ClientGUID, CreateGUID: member.CreateGUID, LeaseKey: member.LeaseKey}); status != smb.StatusObjectNameNotFound {
				t.Fatalf("detached member survived completion: %#x", status)
			}
		})
	}
}

type observedBreakConn struct {
	net.Conn
	started chan struct{}
	armed   atomic.Bool
	once    sync.Once
}

func (conn *observedBreakConn) Write(data []byte) (int, error) {
	if conn.armed.Load() {
		conn.once.Do(func() { close(conn.started) })
	}
	return conn.Conn.Write(data)
}

func TestLeaseBreakBlockedSendCompletesAfterSessionReplacement(t *testing.T) {
	for _, cleanupError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cleanup error"}[cleanupError], func(t *testing.T) {
			checkBlockedLeaseBreakReplacement(t, cleanupError)
		})
	}
}

func checkBlockedLeaseBreakReplacement(t *testing.T, cleanupError bool) {
	t.Helper()
	server, storage := fixedClockLeaseServer(t)
	if cleanupError {
		storage.closeErr = smb.ErrIO
	}
	local, remote := net.Pipe()
	conn := &observedBreakConn{Conn: local, started: make(chan struct{})}
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(errors.Join(err, local.Close(), remote.Close()))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, conn) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case serveErr := <-served:
			if serveErr != nil {
				t.Logf("old transport: %v", serveErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("old transport did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
	conn.armed.Store(true)
	done := startServerBreak(ctx, server, open, smb.LeaseRead|smb.LeaseHandle)
	waitAsyncSignal(ctx, t, conn.started)
	freshClient, freshCtx := pipeClient(t, server)
	fresh, err := freshClient.Login(freshCtx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}, PreviousSessionID: session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	breakErr := waitAsyncResult(ctx, t, done)
	if cleanupError {
		if !errors.Is(breakErr, smb.ErrIO) {
			t.Fatalf("detached cleanup error: %v", breakErr)
		}
	} else if breakErr != nil {
		t.Fatal(breakErr)
	}
	if storage.closed.Load() != 1 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("blocked send kept detached rights or handle")
	}
	// Completion does not cancel the ordered write or discard its captured keys.
	notification, err := client.WaitLeaseBreak(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: [16]byte(open.LeaseKey), Epoch: 8, Flags: 1, CurrentState: smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite, NewState: smb.LeaseRead | smb.LeaseHandle}
	if notification != want {
		t.Fatalf("captured notification: %+v, want %+v", notification, want)
	}
	assertRawStatus(ctx, t, client, echo(t, session.NextMessageID), smb.StatusSuccess)
	if response := exchange(freshCtx, t, freshClient, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("replacement session is not usable")
	}
}
