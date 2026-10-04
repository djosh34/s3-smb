package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

type expiryClock struct {
	now time.Time
	mu  sync.Mutex
}

func (clock *expiryClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *expiryClock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func expiryServer(t *testing.T, storage smb.Storage) (*Server, *expiryClock) {
	t.Helper()
	clock := &expiryClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	options := testOptions(t)
	options.Now = clock.Now
	var err error
	options.State, err = state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return server, clock
}

func expiryRequest(inode smb.Inode, session uint64) state.OpenRequest {
	return state.OpenRequest{
		Object: smb.ObjectKey{Inode: inode}, Binding: state.Binding{SessionID: session, TreeID: 1},
		ClientGUID: state.GUID{1}, CreateGUID: state.GUID{byte(inode & 0xff), byte(session & 0xff)},
		GrantedAccess: 0x10001, Sharing: state.ShareMode(state.RightRead | state.RightDelete),
	}
}

func expiryGrant(request state.OpenRequest) state.Grant {
	return state.Grant{
		Handle: cleanupHandle{object: request.Object}, DurableTimeout: 2 * time.Minute,
		Lease: state.Lease{ClientGUID: request.ClientGUID, Key: state.GUID{byte(request.Object.Inode & 0xff)}, State: smb.LeaseRead | smb.LeaseHandle},
	}
}

func commitExpiryOpen(t *testing.T, server *Server, request state.OpenRequest, grant state.Grant) state.Open {
	t.Helper()
	token, status := server.options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(token, grant)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

func detachExpiryOpen(t *testing.T, server *Server, open state.Open) {
	t.Helper()
	if actions := server.options.State.Disconnect(open.Binding.SessionID); len(actions) != 0 {
		t.Fatalf("durable open closed on disconnect: %+v", actions)
	}
}

func TestExpiryClosesHandlesAndAppliesPendingDeletion(t *testing.T) {
	for _, disposition := range []bool{false, true} {
		t.Run(fmt.Sprintf("set_disposition_%t", disposition), func(t *testing.T) {
			storage := &cleanupStorage{}
			server, clock := expiryServer(t, storage)
			request := expiryRequest(2, 1)
			grant := expiryGrant(request)
			grant.DeleteName = smb.Name{Parent: 1, Base: "old"}
			grant.DeleteOnClose = !disposition
			open := commitExpiryOpen(t, server, request, grant)
			if disposition {
				if status := server.options.State.SetDelete(open.ID, open.Binding, grant.DeleteName, true); status != smb.StatusSuccess {
					t.Fatal(status)
				}
			}
			attachedRequest := expiryRequest(3, 2)
			attached := commitExpiryOpen(t, server, attachedRequest, expiryGrant(attachedRequest))
			detachExpiryOpen(t, server, open)
			clock.advance(open.DurableTimeout - time.Nanosecond)
			server.expire(t.Context())
			if storage.closed.Load() != 0 || storage.removed.Load() != 0 {
				t.Fatal("cleanup ran before the deadline")
			}
			clock.advance(time.Nanosecond)
			server.expire(t.Context())
			server.expire(t.Context())
			server.scavengerCleanup.Wait()
			if storage.closed.Load() != 1 || storage.removed.Load() != 1 {
				t.Fatalf("expiry cleanup: close %d remove %d", storage.closed.Load(), storage.removed.Load())
			}
			if _, status := server.options.State.Find(attached.ID, attached.Binding); status != smb.StatusSuccess {
				t.Fatal("expiry closed an attached durable open")
			}
			if actions := server.options.State.Expire(); len(actions) != 0 {
				t.Fatal("expired open remained in the table")
			}
		})
	}
}

func TestBeforeExpiryRetainsSharingAndRanges(t *testing.T) {
	storage := &cleanupStorage{}
	server, clock := expiryServer(t, storage)
	request := expiryRequest(2, 1)
	open := commitExpiryOpen(t, server, request, expiryGrant(request))
	if status := server.options.State.Lock(open.ID, open.Binding, []state.Range{{Length: 10, Exclusive: true}}, false); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	detachExpiryOpen(t, server, open)
	clock.advance(open.DurableTimeout - time.Nanosecond)
	server.expire(t.Context())
	writer := expiryRequest(2, 2)
	writer.GrantedAccess = 2
	writer.Sharing = 7
	if _, status := server.options.State.Reserve(writer); status != smb.StatusSharingViolation {
		t.Fatalf("detached sharing lost: %#x", status)
	}
	reader := expiryRequest(2, 3)
	readerOpen := commitExpiryOpen(t, server, reader, expiryGrant(reader))
	if status := server.options.State.CheckIO(readerOpen.ID, readerOpen.Binding, 0, 1, false); status != smb.StatusFileLockConflict {
		t.Fatalf("detached range lost: %#x", status)
	}
	if storage.closed.Load() != 0 {
		t.Fatal("detached handle closed before deadline")
	}
}

func TestBreakTimeoutRevokesLeaseAndDurability(t *testing.T) {
	storage := &cleanupStorage{}
	server, clock := expiryServer(t, storage)
	detachedRequest := expiryRequest(2, 1)
	detachedRequest.Sharing = 7
	grant := expiryGrant(detachedRequest)
	grant.Lease.State |= smb.LeaseWrite
	detached := commitExpiryOpen(t, server, detachedRequest, grant)
	attachedRequest := expiryRequest(2, 2)
	attachedRequest.Sharing = 7
	attached := commitExpiryOpen(t, server, attachedRequest, grant)
	detachExpiryOpen(t, server, detached)
	breaks, actions := server.options.State.BreakLeases(detached.Object, state.GUID{9}, state.GUID{}, smb.LeaseRead|smb.LeaseHandle)
	if len(breaks) != 1 || !breaks[0].AckRequired || len(actions) != 0 {
		t.Fatalf("break: %+v cleanup: %+v", breaks, actions)
	}
	clock.advance(state.LeaseBreakTimeout - time.Nanosecond)
	server.expire(t.Context())
	if storage.closed.Load() != 0 {
		t.Fatal("break expired before deadline")
	}
	clock.advance(time.Nanosecond)
	server.expire(t.Context())
	server.expire(t.Context())
	server.scavengerCleanup.Wait()
	if storage.closed.Load() != 1 {
		t.Fatalf("detached member close count: %d", storage.closed.Load())
	}
	open, status := server.options.State.Find(attached.ID, attached.Binding)
	if status != smb.StatusSuccess || open.Durable || open.DurableTimeout != 0 {
		t.Fatalf("attached durability: %+v %#x", open, status)
	}
	// A timeout revokes R and H too, not just the W removed by the target.
	writer := expiryRequest(2, 3)
	writer.GrantedAccess = 2
	writer.Sharing = 7
	if _, _, status := server.options.State.AckBreak(attached.Binding, attached.ClientGUID, attached.LeaseKey, smb.LeaseRead); status != smb.StatusUnsuccessful {
		t.Fatalf("timed-out lease still breaking: %#x", status)
	}
	writer.ClientGUID = state.GUID{9}
	commitExpiryOpen(t, server, writer, state.Grant{Handle: cleanupHandle{object: writer.Object}})
	if status := server.options.State.CheckIO(attached.ID, attached.Binding, 0, 1, false); status != smb.StatusSuccess {
		t.Fatalf("attached member unusable after revocation: %#x", status)
	}
}

func TestExpiryLogsCleanupErrorsAndKeepsSweeping(t *testing.T) {
	storage := &cleanupStorage{closeErr: errors.New("close failed"), removeErr: errors.New("remove failed")}
	server, clock := expiryServer(t, storage)
	var logs bytes.Buffer
	server.options.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	request := expiryRequest(2, 1)
	grant := expiryGrant(request)
	grant.DeleteOnClose = true
	grant.DeleteName = smb.Name{Parent: 1, Base: "old"}
	first := commitExpiryOpen(t, server, request, grant)
	detachExpiryOpen(t, server, first)
	secondRequest := expiryRequest(3, 2)
	second := commitExpiryOpen(t, server, secondRequest, expiryGrant(secondRequest))
	detachExpiryOpen(t, server, second)
	clock.advance(first.DurableTimeout)
	server.expire(t.Context())
	thirdRequest := expiryRequest(4, 3)
	third := commitExpiryOpen(t, server, thirdRequest, expiryGrant(thirdRequest))
	detachExpiryOpen(t, server, third)
	clock.advance(third.DurableTimeout)
	server.expire(t.Context())
	server.scavengerCleanup.Wait()
	if storage.closed.Load() != 3 || storage.removed.Load() != 1 {
		t.Fatalf("cleanup stopped on an error: close %d remove %d", storage.closed.Load(), storage.removed.Load())
	}
	for _, open := range []state.Open{first, second, third} {
		identity := fmt.Sprintf("persistent_id=%d volatile_id=%d inode=%d", open.ID.Persistent, open.ID.Volatile, open.Object.Inode)
		if !strings.Contains(logs.String(), identity) {
			t.Fatalf("missing open identity %s: %s", identity, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "close failed") || !strings.Contains(logs.String(), "remove failed") {
		t.Fatalf("cleanup errors not logged: %s", logs.String())
	}
}
