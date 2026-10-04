package server

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReplacementDrainsSynchronousAndUnpublishedAsyncWork(t *testing.T) {
	for _, command := range []wire.Command{wire.Create, wire.Read} {
		t.Run(fmt.Sprintf("command_%d", command), func(t *testing.T) {
			checkReplacementInFlight(t, command)
		})
	}
}

func checkReplacementInFlight(t *testing.T, command wire.Command) {
	t.Helper()
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	opened := make(chan state.Open, 1)
	server.handlers[command] = func(ctx context.Context, request RequestContext, _ wire.Message) (reply, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
			return reply{}, nil
		}
		<-release
		// A storage completion can publish a grant after cancellation. Cleanup
		// must catch it after draining, not only at the initial disconnect.
		object := smb.ObjectKey{Inode: 34}
		reservation, status := request.Opens.Reserve(state.OpenRequest{Object: object, Binding: request.Binding(), User: request.Session.User, Share: request.Tree.Share, GrantedAccess: 1})
		if status != smb.StatusSuccess {
			return reply{}, fmt.Errorf("late reservation: %#x", status)
		}
		open, status := request.Opens.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}})
		if status != smb.StatusSuccess {
			abortStatus := request.Opens.Abort(reservation)
			return reply{}, fmt.Errorf("late commit: %#x, abort: %#x", status, abortStatus)
		}
		opened <- open
		return reply{}, ctx.Err()
	}
	_, ctx, old := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	owner := onlyConnection(t, server)
	ordinary := insertSessionOpen(t, server, old, false, 32)
	durable := insertSessionOpen(t, server, old, true, 33)
	message := treeRequest(t, old, old.NextMessageID, wire.Read)
	message.Header.Command = command
	var operation *work
	if command == wire.Read {
		// Deliberately stop before waitLocal/sendPending. There is no timer or
		// scheduling race deciding whether this operation is in pending.
		operation = owner.startWork(ctx, message, nil)
	} else {
		operation = &work{done: make(chan struct{})}
		go func() { operation.result = owner.execute(ctx, message); close(operation.done) }()
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	owner.pendingMu.Lock()
	pendingCount := len(owner.pending)
	owner.pendingMu.Unlock()
	if pendingCount != 0 {
		t.Fatal("test operation became pending")
	}
	client, freshCtx := pipeClient(t, server)
	type loginResult struct {
		err     error
		session smbtest.Session
	}
	finished := make(chan loginResult, 1)
	go func() {
		fresh, loginErr := client.Login(freshCtx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}, PreviousSessionID: old.SessionID})
		finished <- loginResult{session: fresh, err: loginErr}
	}()
	select {
	case <-canceled:
	case result := <-finished:
		t.Fatalf("replacement finished without canceling in-flight work: %v", result.err)
	case <-freshCtx.Done():
		t.Fatal(freshCtx.Err())
	}
	select {
	case <-finished:
		t.Fatal("replacement finished before in-flight work drained")
	default:
	}
	if storage.closed.Load() != 0 {
		t.Fatal("storage cleanup raced an active handler")
	}
	for _, open := range []state.Open{ordinary, durable} {
		if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
			t.Fatal("initial disconnect did not detach existing opens promptly")
		}
	}
	releaseOnce.Do(func() { close(release) })
	var result loginResult
	select {
	case result = <-finished:
	case <-freshCtx.Done():
		t.Fatal(freshCtx.Err())
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	<-operation.done
	if operation.result.status != smb.StatusCancelled {
		t.Fatalf("old work: %#x", operation.result.status)
	}
	late := <-opened
	if _, status := options.State.Find(late.ID, late.Binding); status == smb.StatusSuccess {
		t.Fatal("late open was orphaned on the removed session")
	}
	if storage.closed.Load() != 2 {
		t.Fatalf("closed %d handles, want existing and late ordinary opens", storage.closed.Load())
	}
	reservation, status := options.State.Reserve(state.OpenRequest{Object: late.Object, Binding: state.Binding{SessionID: result.session.SessionID, TreeID: result.session.TreeID}, GrantedAccess: 1, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatalf("orphan sharing prevents a fresh open: %#x", status)
	}
	if status := options.State.Abort(reservation); status != smb.StatusSuccess {
		t.Fatalf("fresh reservation cleanup: %#x", status)
	}
	if _, status := options.State.Reconnect(state.ReconnectRequest{ID: durable.ID, Binding: state.Binding{SessionID: result.session.SessionID, TreeID: result.session.TreeID}, User: durable.User, Share: durable.Share, ClientGUID: durable.ClientGUID, CreateGUID: durable.CreateGUID, LeaseKey: durable.LeaseKey}); status != smb.StatusSuccess {
		t.Fatalf("existing durable open was not preserved: %#x", status)
	}
	if result := owner.execute(ctx, message); result.status != smb.StatusUserSessionDeleted {
		t.Fatal("removed identity accepted more work")
	}
}
