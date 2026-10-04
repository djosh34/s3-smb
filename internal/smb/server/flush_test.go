package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type flushBarrier func(context.Context, bool) error

func (barrier flushBarrier) Commit(ctx context.Context, full bool) error {
	return barrier(ctx, full)
}

func flushMessage(t *testing.T, session smbtest.Session, id uint64, file wire.FileID, reserved uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeFlushRequest(wire.FlushRequest{ID: file, Reserved1: reserved})
	if err != nil {
		t.Fatal(err)
	}
	return ioMessage(session, id, wire.Flush, body, 1)
}

func TestFlushCrossHandleWaitsForMetadataBarrier(t *testing.T) {
	for _, reserved := range []uint16{0, 0xffff} {
		t.Run(fmt.Sprintf("reserved_%04x", reserved), func(t *testing.T) {
			checkFlushBarrier(t, reserved)
		})
	}
}

func checkFlushBarrier(t *testing.T, reserved uint16) {
	t.Helper()
	started := make(chan bool, 1)
	resume := make(chan struct{})
	barrier := flushBarrier(func(ctx context.Context, full bool) error {
		select {
		case started <- full:
		default:
		}
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	fixture := newIOFixture(t, barrier)
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	// Always unblock the barrier before connection cleanup, including failures.
	unblock := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(unblock)
	writer := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID, wire.CreateRequest{
		Name: "flush-data", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileCreateDisposition,
	}))
	other := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID+1, wire.CreateRequest{
		Name: "flush-data", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen,
	}))
	session.NextMessageID += 2
	payload := []byte("cross-handle durable bytes")
	writeForFlush(ctx, t, client, session, writer.ID, payload)
	message := flushMessage(t, session, session.NextMessageID+1, other.ID, reserved)
	if sendErr := client.Send(ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case full := <-started:
		if full != (reserved == 0xffff) {
			t.Fatalf("full barrier = %v", full)
		}
	case <-ctx.Done():
		t.Fatal("metadata barrier not reached")
	}
	if fixture.store.puts.Load() == 0 {
		t.Fatal("metadata barrier ran before upload")
	}
	assertCommittedFlushData(t, fixture, payload)
	pending := receiveFlushReply(ctx, t, client)
	if pending.Header.Status != smb.StatusPending {
		t.Fatalf("reply before metadata barrier: %+v", pending.Header)
	}
	// ECHO must be the next reply while the flush barrier remains blocked.
	echoReply := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+2))[0]
	if echoReply.Header.Command != wire.Echo || echoReply.Header.Status != smb.StatusSuccess {
		t.Fatalf("flush completed before barrier: %+v", echoReply.Header)
	}
	unblock()
	final := receiveFlushReply(ctx, t, client)
	if final.Header.Status != smb.StatusSuccess || final.Header.MessageID != message.Header.MessageID {
		t.Fatal(final.Header)
	}
	if _, decodeErr := wire.DecodeFlushResponse(final); decodeErr != nil {
		t.Fatal(decodeErr)
	}
}

func writeForFlush(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id wire.FileID, payload []byte) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(ctx, t, client, ioMessage(session, session.NextMessageID, wire.Write, body, 1))
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	written, err := wire.DecodeWriteResponse(response)
	if err != nil || uint64(written.Count) != uint64(len(payload)) {
		t.Fatalf("write reply = %+v, %v", written, err)
	}
}

func assertCommittedFlushData(t *testing.T, fixture *ioFixture, payload []byte) {
	t.Helper()
	// A native reader uses committed slices, not the adapter's buffered writer.
	file, eno := fixture.native.Open(meta.Background(), "/flush-data", vfs.MODE_MASK_R)
	if eno != 0 {
		t.Fatal(eno)
	}
	data := make([]byte, len(payload))
	n, err := file.Pread(meta.Background(), data, 0)
	closeErr := file.Close(meta.Background())
	if err != nil || closeErr != 0 || n != len(payload) || string(data) != string(payload) {
		t.Fatalf("committed bytes = %q (%d), read %v, close %v", data, n, err, closeErr)
	}
}

func receiveFlushReply(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 || response.Messages[0].Header.Command != wire.Flush {
		t.Fatalf("not one FLUSH reply: %+v", response.Messages)
	}
	return response.Messages[0]
}

func TestFlushStorageErrorsReachClient(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(fmt.Sprintf("upload_%v", upload), func(t *testing.T) {
			var barrierCalls atomic.Int64
			var fail atomic.Bool
			fail.Store(!upload)
			fixture := newIOFixture(t, flushBarrier(func(_ context.Context, full bool) error {
				barrierCalls.Add(1)
				if !full {
					t.Error("FULL_SYNC did not request the full barrier")
				}
				if fail.Load() {
					return syscall.EIO
				}
				return nil
			}))
			options := testOptions(t)
			options.Storage = fixture.adapter
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
			t.Cleanup(func() {
				fail.Store(false)
				fixture.store.fail.Store(false)
			})
			writer := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID, wire.CreateRequest{
				Name: "failed-flush", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileCreateDisposition,
			}))
			other := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID+1, wire.CreateRequest{
				Name: "failed-flush", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen,
			}))
			session.NextMessageID += 2
			fixture.store.fail.Store(upload)
			writeForFlush(ctx, t, client, session, writer.ID, []byte("uncommitted bytes"))
			response := ioRoundTrip(ctx, t, client, flushMessage(t, session, session.NextMessageID+1, other.ID, 0xffff))
			if response.Header.Status != smb.StatusIODeviceError {
				t.Fatalf("storage error = %+v", response.Header)
			}
			if _, decodeErr := wire.DecodeErrorResponse(response); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if fixture.store.puts.Load() == 0 || (barrierCalls.Load() > 0) == upload {
				t.Fatalf("upload/barrier calls = %d/%d", fixture.store.puts.Load(), barrierCalls.Load())
			}
			response = exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+2))[0]
			if response.Header.Status != smb.StatusSuccess {
				t.Fatal("storage error dropped the connection")
			}
		})
	}
}

func TestFlushRejectsInvalidRequestsWithoutStorageWork(t *testing.T) {
	var calls atomic.Int64
	fixture := newIOFixture(t, flushBarrier(func(context.Context, bool) error {
		calls.Add(1)
		return nil
	}))
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	open := createdFile(t, fileCreate(ctx, t, client, session, session.NextMessageID, wire.CreateRequest{
		Name: "invalid-flush", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileCreateDisposition,
	}))
	id := open.ID
	next := session.NextMessageID + 1
	for _, reserved := range []uint16{1, 0x8000, 0xfffe} {
		response := ioRoundTrip(ctx, t, client, flushMessage(t, session, next, id, reserved))
		if response.Header.Status != smb.StatusInvalidParameter {
			t.Fatalf("Reserved1 %04x: %+v", reserved, response.Header)
		}
		next++
	}
	message := flushMessage(t, session, next, id, 0)
	message.Body = message.Body[:10]
	response := ioRoundTrip(ctx, t, client, message)
	if response.Header.Status != smb.StatusInvalidParameter {
		t.Fatal(response.Header)
	}
	next++
	id.Volatile++
	response = ioRoundTrip(ctx, t, client, flushMessage(t, session, next, id, 0))
	if response.Header.Status != smb.StatusFileClosed {
		t.Fatal(response.Header)
	}
	if calls.Load() != 0 || fixture.store.puts.Load() != 0 {
		t.Fatal("invalid FLUSH reached storage")
	}
	response = exchange(ctx, t, client, sessionEcho(t, session, next+1))[0]
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal("invalid FLUSH dropped the connection")
	}
}
