package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestScavengerKeepsRunningAfterCleanupErrors(t *testing.T) {
	storage := &expiryNotifications{cleanupStorage: &cleanupStorage{closeErr: smb.ErrIO}, closedHandles: make(chan smb.Handle, 3)}
	server, clock := expiryServer(t, storage)
	ticks := fakeExpiryTicks(t, server)
	for session := uint64(1); session <= 2; session++ {
		request := expiryRequest(smb.Inode(session), session)
		open := commitExpiryOpen(t, server, request, expiryGrant(request))
		detachExpiryOpen(t, server, open)
	}
	clock.advance(2 * time.Minute)
	sendExpiryTick(t, ticks)
	waitExpiredHandle(t, storage)
	waitExpiredHandle(t, storage)
	request := expiryRequest(3, 3)
	open := commitExpiryOpen(t, server, request, expiryGrant(request))
	detachExpiryOpen(t, server, open)
	clock.advance(open.DurableTimeout)
	sendExpiryTick(t, ticks)
	waitExpiredHandle(t, storage)
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.scavengerDone:
	default:
		t.Fatal("shutdown left the scavenger running")
	}
	select {
	case ticks <- time.Time{}:
		t.Fatal("scavenger took a tick after shutdown")
	default:
	}
}

func TestShutdownWaitsForScavengerCleanup(t *testing.T) {
	storage := &blockedCleanup{cleanupStorage: &cleanupStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	server, clock := expiryServer(t, storage)
	ticks := fakeExpiryTicks(t, server)
	request := expiryRequest(2, 1)
	open := commitExpiryOpen(t, server, request, expiryGrant(request))
	detachExpiryOpen(t, server, open)
	clock.advance(open.DurableTimeout)
	sendExpiryTick(t, ticks)
	select {
	case <-storage.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("scavenger never started cleanup")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled shutdown: %v", err)
	}
	select {
	case <-server.shutdownDone:
		t.Fatal("shutdown returned before expiry cleanup finished")
	default:
	}
	close(storage.release)
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 1 {
		t.Fatalf("expiry cleanup count: %d", storage.closed.Load())
	}
}

func TestScavengerIsSharedAndShutdownClosesAllOpens(t *testing.T) {
	storage := &cleanupStorage{}
	server, _ := expiryServer(t, storage)
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	exchange(ctx, t, client, echo(t, 1))
	stop, done := server.scavengerStop, server.scavengerDone
	other, otherCtx := corePipeClient(t, server)
	exchange(otherCtx, t, other, negotiateMessage(t, 1))
	exchange(otherCtx, t, other, echo(t, 1))
	if stop == nil || done == nil || stop != server.scavengerStop || done != server.scavengerDone {
		t.Fatal("connections do not share one scavenger")
	}
	request := expiryRequest(2, 1)
	grant := expiryGrant(request)
	grant.DeleteOnClose = true
	grant.DeleteName = smb.Name{Parent: 1, Base: "old"}
	detached := commitExpiryOpen(t, server, request, grant)
	detachExpiryOpen(t, server, detached)
	attachedRequest := expiryRequest(3, 2)
	commitExpiryOpen(t, server, attachedRequest, expiryGrant(attachedRequest))
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("shutdown did not stop the timer")
	}
	if storage.closed.Load() != 2 || storage.removed.Load() != 1 {
		t.Fatalf("shutdown cleanup: close %d remove %d", storage.closed.Load(), storage.removed.Load())
	}
	if actions := server.options.State.CloseAll(); len(actions) != 0 {
		t.Fatal("shutdown left opens in the table")
	}
}
