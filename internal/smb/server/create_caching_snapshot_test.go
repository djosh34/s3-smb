package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type createSnapshotStorage struct {
	smb.Storage
	entered chan struct{}
	release chan struct{}
	block   atomic.Bool
}

func (storage *createSnapshotStorage) GetAttr(ctx context.Context, object smb.ObjectKey) (smb.Attr, error) {
	if storage.block.CompareAndSwap(true, false) {
		close(storage.entered)
		select {
		case <-storage.release:
		case <-ctx.Done():
			return smb.Attr{}, ctx.Err()
		}
	}
	return storage.Storage.GetAttr(ctx, object)
}

func TestReplayCachingResponseUsesFreshEffectiveH(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprintf("acknowledged-%t", acknowledged), func(t *testing.T) {
			checkReplayCachingResponseUsesFreshEffectiveH(t, acknowledged)
		})
	}
}

func checkReplayCachingResponseUsesFreshEffectiveH(t *testing.T, acknowledged bool) {
	t.Helper()
	options := testOptions(t)
	storage := &createSnapshotStorage{Storage: newFilesMetaStorage(t), entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(storage.release) })
	t.Cleanup(unblock)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	holder, writer := loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3)
	create := durableCreateOptions()
	create.Request.Name, create.Request.DesiredAccess, create.Request.ShareAccess = "file", fileReadData, 1
	create.Lease = leaseV2(4, smb.LeaseRead|smb.LeaseHandle)
	header := holder.header(wire.Create)
	if sendErr := holder.client.SendCreate(holder.ctx, header, create); sendErr != nil {
		t.Fatal(sendErr)
	}
	initial := holder.created(t, header.MessageID)
	if initial.Durable == nil || initial.Durable.Timeout != 120000 {
		t.Fatalf("initial durable response = %+v", initial)
	}

	// A second attached member can ACK while the original transport
	// is serving the blocked replay. No connection policy change.
	acknowledger := loginCreateLeaseClient(t, server, 2)
	member := acknowledger.create(t, leaseCreateRequest("file"), leaseV2(4, smb.LeaseRead))
	storage.block.Store(true)
	replay := holder.header(wire.Create)
	replay.Flags |= wire.FlagReplay
	if sendErr := holder.client.SendCreate(holder.ctx, replay, create); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-storage.entered:
	case <-holder.ctx.Done():
		t.Fatal(holder.ctx.Err())
	}
	competing := leaseCreateRequest("file")
	competing.DesiredAccess = fileWriteData
	id := writer.send(t, competing, nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.CurrentState != smb.LeaseRead|smb.LeaseHandle || notification.NewState != smb.LeaseRead || notification.Epoch != 9 {
		t.Fatalf("H-removing break = %+v", notification)
	}
	if acknowledged {
		acknowledger.ack(t, notification)
	}
	unblock()
	result := holder.created(t, replay.MessageID)
	wantState, wantFlags := uint32(smb.LeaseRead|smb.LeaseHandle), uint32(leaseParentKeySet|leaseBreakInProgress)
	if acknowledged {
		wantState, wantFlags = smb.LeaseRead, leaseParentKeySet
	}
	if result.Reply.ID != initial.Reply.ID || result.Lease == nil || result.Lease.State != wantState || result.Lease.Epoch != 9 || result.Lease.Flags != wantFlags || result.Durable != nil {
		t.Fatalf("replay exposed stale caching/durability: %+v, lease %+v", result, result.Lease)
	}
	if !acknowledged {
		acknowledger.ack(t, notification)
	}
	refused, err := writer.receive(id)
	if err != nil || refused.Header.Status != smb.StatusSharingViolation {
		t.Fatalf("sharing retry = %#x, error %v", refused.Header.Status, err)
	}
	holder.close(t, initial.Reply.ID)
	acknowledger.close(t, member.Reply.ID)
}
