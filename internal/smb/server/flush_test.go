package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFlushCrossHandleWaitsForMetadataBarrier(t *testing.T) {
	for _, reserved := range []uint16{0, 0xffff} {
		t.Run(fmt.Sprintf("reserved_%04x", reserved), func(t *testing.T) {
			checkFlushBarrier(t, reserved)
		})
	}
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
