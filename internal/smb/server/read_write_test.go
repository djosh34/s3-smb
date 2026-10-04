package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type readWriteClient struct {
	client  *smbtest.Client
	server  *Server
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

func newReadWriteClient(t *testing.T, storage smb.Storage) *readWriteClient {
	t.Helper()
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	// Grow the window before exercising maximum-size multi-credit operations.
	message := sessionEcho(t, session, session.NextMessageID)
	message.Header.Credit = 32
	if response := ioRoundTrip(ctx, t, client, message); response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	return &readWriteClient{client: client, server: server, ctx: ctx, session: session, next: session.NextMessageID + 1}
}

func (client *readWriteClient) read(t *testing.T, request wire.ReadRequest, charge uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeReadRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return client.exchange(t, wire.Read, body, charge)
}

func (client *readWriteClient) write(t *testing.T, request wire.WriteRequest, charge uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeWriteRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return client.exchange(t, wire.Write, body, charge)
}

func (client *readWriteClient) exchange(t *testing.T, command wire.Command, body []byte, charge uint16) wire.Message {
	t.Helper()
	message := ioMessage(client.session, client.next, command, body, charge)
	client.next += uint64(max(charge, 1))
	return ioRoundTrip(client.ctx, t, client.client, message)
}

func requireIOStatus(t *testing.T, response wire.Message, want smb.Status) {
	t.Helper()
	if response.Header.Status != want {
		t.Fatalf("status %#x, want %#x", response.Header.Status, want)
	}
}

func TestReadSeesWriteFromAnotherOpen(t *testing.T) {
	fixture := newIOFixture(t, nil)
	client := newReadWriteClient(t, fixture.adapter)
	writer := insertIOOpen(t, client.server, client.session, "coherent", fileWriteData)
	reader := insertIOOpen(t, client.server, client.session, "coherent", fileReadData)
	response := client.write(t, wire.WriteRequest{ID: wire.FileID(writer.ID), Offset: 2, Data: []byte("hello")}, 1)
	requireIOStatus(t, response, smb.StatusSuccess)
	written, err := wire.DecodeWriteResponse(response)
	if err != nil || written.Count != 5 || written.Remaining != 0 {
		t.Fatalf("write response: %+v, %v", written, err)
	}
	for _, test := range []struct {
		name, data      string
		offset          uint64
		length, minimum uint32
		want            smb.Status
	}{
		{"short read", "hello", 2, 10, 0, smb.StatusSuccess},
		{"minimum met", "hello", 2, 10, 5, smb.StatusSuccess},
		{"minimum unmet", "", 2, 10, 6, smb.StatusEndOfFile},
		{"minimum exceeds request", "", 2, 3, 4, smb.StatusEndOfFile},
		{"at EOF", "", 7, 1, 0, smb.StatusEndOfFile},
		{"past EOF", "", 8, 1, 0, smb.StatusEndOfFile},
		{"zero length", "", 7, 0, 0, smb.StatusSuccess},
		{"zero length minimum", "", 7, 0, 1, smb.StatusEndOfFile},
		{"hole", "\x00\x00", 0, 2, 0, smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := client.read(t, wire.ReadRequest{ID: wire.FileID(reader.ID), Offset: test.offset, Length: test.length, MinimumCount: test.minimum}, 1)
			requireIOStatus(t, response, test.want)
			if test.want == smb.StatusSuccess {
				read, err := wire.DecodeReadResponse(response)
				if err != nil || string(read.Data) != test.data || read.Remaining != 0 {
					t.Fatalf("read response: %+v, %v", read, err)
				}
			}
		})
	}
}

func TestReadWriteGrantedAccess(t *testing.T) {
	fixture := newIOFixture(t, nil)
	client := newReadWriteClient(t, fixture.adapter)
	for _, mask := range []uint32{0, 0x80, fileReadData, fileWriteData, fileAppendData} {
		t.Run(fmt.Sprintf("mask_%x", mask), func(t *testing.T) {
			open := insertIOOpen(t, client.server, client.session, fmt.Sprintf("access-%x", mask), mask)
			readWant, writeWant := smb.StatusAccessDenied, smb.StatusAccessDenied
			if mask&fileReadData != 0 {
				readWant = smb.StatusSuccess
			}
			if mask&(fileWriteData|fileAppendData) != 0 {
				writeWant = smb.StatusSuccess
			}
			requireIOStatus(t, client.read(t, wire.ReadRequest{ID: wire.FileID(open.ID)}, 1), readWant)
			requireIOStatus(t, client.write(t, wire.WriteRequest{ID: wire.FileID(open.ID), Data: []byte("first")}, 1), writeWant)
			if mask == fileAppendData {
				requireIOStatus(t, client.write(t, wire.WriteRequest{ID: wire.FileID(open.ID), Data: []byte("bad")}, 1), smb.StatusAccessDenied)
				requireIOStatus(t, client.write(t, wire.WriteRequest{ID: wire.FileID(open.ID), Offset: 5, Data: []byte("append")}, 1), smb.StatusSuccess)
			}
		})
	}
}

func TestReadWriteExclusiveLockConflict(t *testing.T) {
	fixture := newIOFixture(t, nil)
	client := newReadWriteClient(t, fixture.adapter)
	owner := insertIOOpen(t, client.server, client.session, "locked", 3)
	other := insertIOOpen(t, client.server, client.session, "locked", 3)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: wire.FileID(owner.ID), Data: []byte("locked")}, 1), smb.StatusSuccess)
	if status := client.server.options.State.Lock(owner.ID, owner.Binding, []state.Range{{Offset: 0, Length: 6, Exclusive: true}}, false); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: wire.FileID(other.ID), Length: 6}, 1), smb.StatusFileLockConflict)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: wire.FileID(other.ID), Data: []byte("bad")}, 1), smb.StatusFileLockConflict)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: wire.FileID(owner.ID), Length: 6}, 1), smb.StatusSuccess)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: wire.FileID(owner.ID), Data: []byte("own")}, 1), smb.StatusSuccess)
}

func TestReadWriteLimitsAndChannels(t *testing.T) {
	fixture := newIOFixture(t, nil)
	client := newReadWriteClient(t, fixture.adapter)
	open := insertIOOpen(t, client.server, client.session, "limits", 3)
	id := wire.FileID(open.ID)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: id, Data: make([]byte, smb.MaxWriteSize)}, 16), smb.StatusSuccess)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: id, Length: smb.MaxReadSize}, 16), smb.StatusSuccess)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: id, Data: make([]byte, smb.MaxWriteSize+1)}, 17), smb.StatusInvalidParameter)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: id, Length: smb.MaxReadSize + 1}, 17), smb.StatusInvalidParameter)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: id, Length: smb.CreditUnit + 1}, 1), smb.StatusInvalidParameter)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: id, Data: make([]byte, smb.CreditUnit+1)}, 1), smb.StatusInvalidParameter)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: id, Channel: 1}, 1), smb.StatusInvalidParameter)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: id, Channel: 1}, 1), smb.StatusInvalidParameter)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: id, Offset: math.MaxUint64, Length: 2}, 1), smb.StatusInvalidParameter)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: id, Offset: math.MaxUint64, Data: []byte("x")}, 1), smb.StatusInvalidParameter)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: id}, 1), smb.StatusSuccess)
}

type writeBarrier struct {
	entered, resume chan struct{}
	once            sync.Once
	full            bool
}

func (barrier *writeBarrier) Commit(ctx context.Context, full bool) error {
	barrier.once.Do(func() { barrier.full = full; close(barrier.entered) })
	select {
	case <-barrier.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWriteThroughWaitsForDurableStorage(t *testing.T) {
	barrier := &writeBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	var unblock sync.Once
	fixture := newIOFixture(t, barrier)
	client := newReadWriteClient(t, fixture.adapter)
	t.Cleanup(func() { unblock.Do(func() { close(barrier.resume) }) })
	open := insertIOOpen(t, client.server, client.session, "durable", fileWriteData)
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: wire.FileID(open.ID), Data: []byte("durable"), Flags: writeThrough})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.Write, body, 1)
	if sendErr := client.client.Send(client.ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-barrier.entered:
	case <-client.ctx.Done():
		t.Fatal(client.ctx.Err())
	}
	if barrier.full || fixture.store.puts.Load() == 0 {
		t.Fatal("WRITE_THROUGH did not upload data with SyncData")
	}
	pending, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, pending.Messages[0], smb.StatusPending)
	unblock.Do(func() { close(barrier.resume) })
	final, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, final.Messages[0], smb.StatusSuccess)
	written, err := wire.DecodeWriteResponse(final.Messages[0])
	if err != nil || written.Count != 7 {
		t.Fatalf("write through: %+v, %v", written, err)
	}
}

type failedIOStorage struct {
	smb.Storage
	readErr, writeErr, flushErr error
	readCount, writeCount       int
	flushes                     int
}

func (storage *failedIOStorage) ReadAt(_ context.Context, _ smb.Handle, data []byte, _ uint64) (int, error) {
	copy(data, "data")
	return storage.readCount, storage.readErr
}

func (storage *failedIOStorage) WriteAt(_ context.Context, _ smb.Handle, _ []byte, _ uint64) (int, error) {
	return storage.writeCount, storage.writeErr
}

func (storage *failedIOStorage) Flush(_ context.Context, _ smb.Handle, _ smb.SyncMode) error {
	storage.flushes++
	return storage.flushErr
}

func TestReadWriteStorageFailures(t *testing.T) {
	for _, test := range []struct {
		readErr, writeErr, flushErr error
		name                        string
		readCount, writeCount       int
		flushes                     int
		want                        smb.Status
		command                     wire.Command
	}{
		{name: "read error with bytes", command: wire.Read, readCount: 2, readErr: smb.ErrIO, want: smb.StatusIODeviceError},
		{name: "EOF with bytes", command: wire.Read, readCount: 2, readErr: io.EOF, want: smb.StatusSuccess},
		{name: "storage category beats EOF", command: wire.Read, readCount: 2, readErr: errors.Join(smb.ErrIO, io.EOF), want: smb.StatusIODeviceError},
		{name: "invalid read count", command: wire.Read, readCount: 5, want: smb.StatusIODeviceError},
		{name: "write error", command: wire.Write, writeErr: smb.ErrDiskFull, want: smb.StatusDiskFull},
		{name: "short write", command: wire.Write, writeCount: 2, want: smb.StatusIODeviceError},
		{name: "flush error", command: wire.Write, writeCount: 4, flushErr: smb.ErrIO, want: smb.StatusIODeviceError, flushes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIOFixture(t, nil)
			storage := &failedIOStorage{Storage: fixture.adapter, readCount: test.readCount, writeCount: test.writeCount, readErr: test.readErr, writeErr: test.writeErr, flushErr: test.flushErr}
			client := newReadWriteClient(t, storage)
			open := insertIOOpen(t, client.server, client.session, "failures", 3)
			var response wire.Message
			if test.command == wire.Read {
				response = client.read(t, wire.ReadRequest{ID: wire.FileID(open.ID), Length: 4}, 1)
			} else {
				response = client.write(t, wire.WriteRequest{ID: wire.FileID(open.ID), Data: []byte("data"), Flags: writeThrough}, 1)
			}
			requireIOStatus(t, response, test.want)
			if storage.flushes != test.flushes {
				t.Fatalf("flush calls: %d, want %d", storage.flushes, test.flushes)
			}
		})
	}
}
