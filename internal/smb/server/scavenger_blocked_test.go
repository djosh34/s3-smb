package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type delayedExpiryClose struct {
	*expiryNotifications
	entered chan struct{}
	release chan struct{}
}

func (storage *delayedExpiryClose) Close(ctx context.Context, handle smb.Handle) error {
	if handle.Key().Inode == 2 {
		close(storage.entered)
		<-storage.release
	}
	return storage.expiryNotifications.Close(ctx, handle)
}

func TestScavengerExpiresWhileCleanupIsBlocked(t *testing.T) {
	for _, block := range []string{"parent", "close", "open use"} {
		t.Run(block, func(t *testing.T) { checkExpiryWhileCleanupBlocked(t, block) })
	}
}

func checkExpiryWhileCleanupBlocked(t *testing.T, block string) {
	t.Helper()
	storage := &expiryNotifications{cleanupStorage: &cleanupStorage{}, closedHandles: make(chan smb.Handle, 3)}
	delayed := &delayedExpiryClose{expiryNotifications: storage, entered: make(chan struct{}), release: make(chan struct{})}
	var backend smb.Storage = storage
	if block == "close" {
		backend = delayed
	}
	server, clock := expiryServer(t, backend)
	ticks := fakeExpiryTicks(t, server)
	request := expiryRequest(2, 1)
	grant := expiryGrant(request)
	grant.DeleteOnClose = true
	grant.DeleteName = smb.Name{Parent: 1, Base: "old"}
	first := commitExpiryOpen(t, server, request, grant)
	unblock := blockExpiryCleanup(t, server, first, block, delayed)
	detachExpiryOpen(t, server, first)
	clock.advance(first.DurableTimeout)
	sendExpiryTick(t, ticks)
	if block == "parent" {
		waitExpiredHandle(t, storage)
	}
	if block == "close" {
		select {
		case <-delayed.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("expiry did not enter blocked Close")
		}
	}
	// This lease deadline starts only after the earlier expiry pass began.
	holderRequest := expiryRequest(3, 2)
	holderGrant := expiryGrant(holderRequest)
	holderGrant.Lease.State |= smb.LeaseWrite
	holder := commitExpiryOpen(t, server, holderRequest, holderGrant)
	breaks, actions := server.options.State.BreakLeases(holder.Object, state.GUID{9}, state.GUID{}, smb.LeaseRead|smb.LeaseHandle)
	if len(breaks) != 1 || !breaks[0].AckRequired || len(actions) != 0 {
		t.Fatalf("later break: %+v, cleanup: %+v", breaks, actions)
	}
	laterRequest := expiryRequest(4, 3)
	laterGrant := expiryGrant(laterRequest)
	laterGrant.DurableTimeout = state.LeaseBreakTimeout
	later := commitExpiryOpen(t, server, laterRequest, laterGrant)
	detachExpiryOpen(t, server, later)
	clock.advance(state.LeaseBreakTimeout)
	sendExpiryTick(t, ticks)
	// Taking the next tick proves the previous table-only pass has finished.
	sendExpiryTick(t, ticks)
	found, status := server.options.State.Find(holder.ID, holder.Binding)
	if status != smb.StatusSuccess || found.Durable || found.DurableTimeout != 0 {
		t.Fatalf("blocked cleanup delayed break revocation: %+v, status %#x", found, status)
	}
	if _, status := server.options.State.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, smb.LeaseRead); status != smb.StatusUnsuccessful {
		t.Fatalf("timed-out break still pending: %#x", status)
	}
	waitExpiredHandle(t, storage)
	if storage.removed.Load() != 0 {
		t.Fatal("blocked expiry deletion ran before cleanup was released")
	}
	assertShutdownDrainsBlockedExpiry(t, server)
	unblock()
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 3 || storage.removed.Load() != 1 {
		t.Fatalf("shutdown did not drain all cleanup: close %d, remove %d", storage.closed.Load(), storage.removed.Load())
	}
}

func blockExpiryCleanup(t *testing.T, server *Server, open state.Open, block string, storage *delayedExpiryClose) func() {
	t.Helper()
	var release func()
	switch block {
	case "parent":
		release = lockParent(RequestContext{server: server}, 1)
	case "close":
		release = func() { close(storage.release) }
	case "open use":
		_, releaseOpen, status := useOpen(openRequestContext(server, open), wire.FileID(open.ID))
		if status != smb.StatusSuccess {
			t.Fatal(status)
		}
		release = releaseOpen
	default:
		t.Fatalf("unknown cleanup blocker %q", block)
	}
	var once sync.Once
	unblock := func() { once.Do(release) }
	t.Cleanup(unblock)
	return unblock
}

func assertShutdownDrainsBlockedExpiry(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled shutdown: %v", err)
	}
	select {
	case <-server.scavengerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked cleanup prevented the expiry timer from stopping")
	}
	select {
	case <-server.shutdownDone:
		t.Fatal("shutdown finished while an expiry cleanup was blocked")
	default:
	}
}
