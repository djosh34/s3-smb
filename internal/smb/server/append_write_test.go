package server

import (
	"context"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Pause after handler authorization, before the real adapter takes its inode lock.
type pausedAppendStorage struct {
	smb.Storage
	entered, resume chan struct{}
	once            sync.Once
}

func (storage *pausedAppendStorage) WriteAt(ctx context.Context, handle smb.Handle, data []byte, offset uint64) (int, error) {
	if string(data) == "stale" {
		storage.once.Do(func() { close(storage.entered) })
		select {
		case <-storage.resume:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return storage.Storage.WriteAt(ctx, handle, data, offset)
}

func TestAppendWriteRechecksEOFAfterCompetingWrite(t *testing.T) {
	for _, access := range []struct {
		name string
		mask uint32
	}{
		{"ordinary writer", fileReadData | fileWriteData},
		{"append writer", fileReadData | fileAppendData},
	} {
		t.Run(access.name, func(t *testing.T) {
			fixture := newIOFixture(t, nil)
			storage := &pausedAppendStorage{Storage: fixture.adapter, entered: make(chan struct{}), resume: make(chan struct{})}
			client := newReadWriteClient(t, storage)
			var unblock sync.Once
			t.Cleanup(func() { unblock.Do(func() { close(storage.resume) }) })
			initial := createdFile(t, client.create(t, createRequest("append", fileCreateDisposition)))
			writeCreatedFile(t, client, initial.ID, "seed")
			request := createRequest("append", fileOpen)
			request.DesiredAccess = fileReadData | fileAppendData
			appender := createdFile(t, client.create(t, request))
			request.DesiredAccess = access.mask
			winner := createdFile(t, client.create(t, request))
			other := createdFile(t, client.create(t, createRequest("unrelated", fileCreateDisposition)))

			body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: appender.ID, Offset: 4, Data: []byte("stale")})
			if err != nil {
				t.Fatal(err)
			}
			message := ioMessage(client.session, client.next, wire.Write, body, 1)
			client.next++
			if err = client.client.Send(client.ctx, []wire.Message{message}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-storage.entered:
			case <-client.ctx.Done():
				t.Fatal(client.ctx.Err())
			}
			pending, err := client.client.Receive(client.ctx)
			if err != nil || len(pending.Messages) != 1 {
				t.Fatalf("pending response: %+v, %v", pending, err)
			}
			requireIOStatus(t, pending.Messages[0], smb.StatusPending)
			requireIOStatus(t, client.write(t, wire.WriteRequest{ID: winner.ID, Offset: 4, Data: []byte("winner")}, 1), smb.StatusSuccess)
			writeCreatedFile(t, client, other.ID, "other")
			readCreatedFile(t, client, other.ID, "other")
			unblock.Do(func() { close(storage.resume) })
			final, err := client.client.Receive(client.ctx)
			if err != nil || len(final.Messages) != 1 {
				t.Fatalf("final response: %+v, %v", final, err)
			}
			readCreatedFile(t, client, initial.ID, "seedwinner")
			requireIOStatus(t, final.Messages[0], smb.StatusAccessDenied)
			if final.Messages[0].Header.MessageID != message.Header.MessageID || final.Messages[0].Header.AsyncID != pending.Messages[0].Header.AsyncID {
				t.Fatal("append completion lost its request identity")
			}
		})
	}
}

func TestAppendSupersedeInitializesWithoutGrantingOverwrite(t *testing.T) {
	client := newReadWriteClient(t, newIOFixture(t, nil).adapter)
	initial := createdFile(t, client.create(t, createRequest("supersede", fileCreateDisposition)))
	writeCreatedFile(t, client, initial.ID, "old data")
	request := createRequest("supersede", fileSupersede)
	request.DesiredAccess = fileDelete | fileReadData | fileAppendData
	appender := createdFile(t, client.create(t, request))
	if appender.Size != 0 || appender.Action != 0 {
		t.Fatalf("supersede: %+v", appender)
	}
	readCreatedFile(t, client, initial.ID, "")
	writeCreatedFile(t, client, appender.ID, "new")
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: appender.ID, Data: []byte("bad")}, 1), smb.StatusAccessDenied)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: appender.ID, Offset: 3, Data: []byte(" tail")}, 1), smb.StatusSuccess)
	readCreatedFile(t, client, initial.ID, "new tail")
}
