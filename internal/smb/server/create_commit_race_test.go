package server

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Count actual adapter references: reselection must not repeat storage Open or
// close a successfully committed handle as though it were a failed CREATE.
type createCommitRaceStorage struct {
	smb.Storage
	opens  atomic.Uint64
	closes atomic.Uint64
}

func (storage *createCommitRaceStorage) Open(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	handle, err := storage.Storage.Open(ctx, object, access)
	if err == nil {
		storage.opens.Add(1)
	}
	return handle, err
}

func (storage *createCommitRaceStorage) Close(ctx context.Context, handle smb.Handle) error {
	err := storage.Storage.Close(ctx, handle)
	if err == nil {
		storage.closes.Add(1)
	}
	return err
}

func TestConcurrentCreateAndMutationNeverExposeStaleGrantInvalidParameter(t *testing.T) {
	options := testOptions(t)
	storage := &createCommitRaceStorage{Storage: newFilesMetaStorage(t)}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	writer, reader := loginCreateLeaseClient(t, server, 3), loginCreateLeaseClient(t, server, 4)
	ordinary := leaseCreateRequest("file")
	ordinary.DesiredAccess = fileWriteData
	mutator := writer.create(t, ordinary, nil)
	id := state.FileID{Persistent: mutator.Reply.ID.Persistent, Volatile: mutator.Reply.ID.Volatile}
	binding := state.Binding{SessionID: writer.session.SessionID, TreeID: writer.session.TreeID}

	// Exercise the real public Begin/End module against protected wire CREATEs.
	// No bytes are mutated here: notification-delivery/readiness and actual
	// WriteAt/Flush lifetime remain separate operation-hook regressions.
	stop, ready, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	stopOnce := sync.OnceFunc(func() { close(stop) })
	var cycles atomic.Uint64
	go func() {
		for index := range 1000000 {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			mutation, _, _, status := options.State.BeginMutation(id, binding)
			if status != smb.StatusSuccess {
				done <- fmt.Errorf("BeginMutation status %#x", status)
				return
			}
			if index == 0 {
				close(ready)
			}
			runtime.Gosched()
			options.State.EndMutation(mutation)
			cycles.Add(1)
		}
		done <- fmt.Errorf("bounded mutation race exhausted before CREATEs finished")
	}()
	join := sync.OnceFunc(func() {
		stopOnce()
		if mutationErr := <-done; mutationErr != nil {
			t.Error(mutationErr)
		}
	})
	t.Cleanup(join)
	select {
	case <-ready:
	case <-reader.ctx.Done():
		t.Fatal(reader.ctx.Err())
	}
	for index := range 100 {
		create := durableCreateOptions()
		create.Request = leaseCreateRequest("file")
		create.Request.Disposition, create.Request.Options = fileOpen, fileWriteThrough
		create.Lease = leaseV2(byte(index+1), smb.LeaseRead|smb.LeaseHandle)
		create.Durable.CreateGUID = [16]byte{byte(index + 1), 5}
		header := reader.header(wire.Create)
		if sendErr := reader.client.SendCreate(reader.ctx, header, create); sendErr != nil {
			t.Fatal(sendErr)
		}
		result := reader.created(t, header.MessageID)
		checkConcurrentCreateGrant(t, result.Lease, result.Durable, result.Reply.OplockLevel)
		open, status := options.State.Find(state.FileID{Persistent: result.Reply.ID.Persistent, Volatile: result.Reply.ID.Volatile}, state.Binding{SessionID: reader.session.SessionID, TreeID: reader.session.TreeID})
		if status != smb.StatusSuccess || !open.WriteThrough || open.CreateAction != 1 {
			t.Fatalf("concurrent CREATE lost mode/storage identity: %+v, status %#x", open, status)
		}
		if result.Lease.State == 0 && (open.LeaseKey != (state.GUID{}) || open.Durable || open.DurableTimeout != 0) {
			t.Fatalf("declined response retained unsafe grant: %+v", open)
		}
		reader.close(t, result.Reply.ID)
	}
	join()
	writer.close(t, mutator.Reply.ID)
	if storage.opens.Load() != 101 || storage.closes.Load() != 101 || cycles.Load() == 0 {
		t.Fatalf("CREATE reselection repeated/leaked storage or lacked races: opens %d, closes %d, mutations %d", storage.opens.Load(), storage.closes.Load(), cycles.Load())
	}
	t.Logf("100 valid protected CREATEs raced %d real mutation gates; mode, epochs, effective-H responses and one Open/Close per request verified", cycles.Load())
}

func checkConcurrentCreateGrant(t *testing.T, lease *wire.LeaseContext, durable *wire.DurableReply, level uint8) {
	t.Helper()
	if lease == nil || lease.ParentKey != [16]byte{8} || lease.Duration != 0 {
		t.Fatalf("invalid concurrent lease response: %+v", lease)
	}
	if lease.State == 0 {
		if level != 0 || lease.Epoch != 7 || lease.Flags != leaseParentKeySet || durable != nil {
			t.Fatalf("declined response advanced epoch/promised durability: %+v, %+v", lease, durable)
		}
		return
	}
	if lease.State != smb.LeaseRead|smb.LeaseHandle || level != leaseOplockLevel {
		t.Fatalf("unsupported concurrent grant: %+v, level %#x", lease, level)
	}
	if lease.Flags == leaseParentKeySet|leaseBreakInProgress {
		if lease.Epoch != 9 || durable != nil {
			t.Fatalf("pending NONE target promised effective H: %+v, %+v", lease, durable)
		}
		return
	}
	if lease.Flags != leaseParentKeySet || lease.Epoch != 8 || durable != nil && durable.Timeout != 120000 {
		t.Fatalf("acquisition changed shared epoch/timeout contract: %+v, %+v", lease, durable)
	}
}
