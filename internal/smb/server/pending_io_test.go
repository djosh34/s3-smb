package server

import (
	"bytes"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSlowS3PendingIO(t *testing.T) {
	for _, command := range []wire.Command{wire.Read, wire.Write, wire.Flush} {
		t.Run(commandName(command), func(t *testing.T) {
			fixture, proxy := newPendingIOFixture(t, false)
			server, client, ctx, session := pendingClient(t, fixture, 15*time.Second)
			open := insertIOOpen(t, server, session, "slow", 3)
			cached := insertIOOpen(t, server, session, "cached", 3)
			cacheData := []byte("unrelated cached bytes")
			seedPendingData(ctx, t, fixture, cached, cacheData)
			warmPendingRead(ctx, t, fixture, cached, cacheData)
			// Two credits also test replenishment for multi-credit I/O.
			data := bytes.Repeat([]byte("slow storage bytes\n"), 4000)
			request := pendingWrite(t, session, session.NextMessageID, open, data)
			method := http.MethodPut
			switch uint16(command) {
			case uint16(wire.Read):
				seedPendingData(ctx, t, fixture, open, data)
				request = pendingRead(t, session, session.NextMessageID, open, 0, pendingLength(t, data))
				method = http.MethodGet
			case uint16(wire.Flush):
				data = data[:32]
				if _, err := fixture.adapter.WriteAt(ctx, open.Handle, data, 0); err != nil {
					t.Fatal(err)
				}
				request = flushMessage(t, session, session.NextMessageID, wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, 0)
			}
			const delay = 750 * time.Millisecond
			if err := proxy.SetFault(s3fault.Fault{Method: method, HeaderDelay: delay}); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if err := client.Send(ctx, []wire.Message{request}); err != nil {
				t.Fatal(err)
			}
			interim := receivePendingIO(ctx, t, client)
			assertInterimIO(t, request, interim)
			assertUnrelatedIO(ctx, t, client, session, cached, request.Header.MessageID+uint64(request.Header.CreditCharge), cacheData)
			if time.Since(start) >= delay {
				t.Fatal("unrelated requests did not complete during the S3 delay")
			}
			final := receivePendingIO(ctx, t, client)
			assertFinalIO(t, interim, final, smb.StatusSuccess)
			if time.Since(start) < delay {
				t.Fatal("storage request completed before S3 replied")
			}
			assertIOSuccess(t, command, final, data)
		})
	}
}

func TestS3RetryErrorKeepsAsyncIdentity(t *testing.T) {
	for _, command := range []wire.Command{wire.Read, wire.Write, wire.Flush} {
		t.Run(commandName(command), func(t *testing.T) {
			fixture, proxy := newPendingIOFixture(t, false)
			server, client, ctx, session := pendingClient(t, fixture, 15*time.Second)
			open := insertIOOpen(t, server, session, "failed", 3)
			data := []byte("failed upload")
			request := pendingWrite(t, session, session.NextMessageID, open, data)
			method := http.MethodPut
			switch uint16(command) {
			case uint16(wire.Read):
				seedPendingData(ctx, t, fixture, open, data)
				request = pendingRead(t, session, session.NextMessageID, open, 0, pendingLength(t, data))
				method = http.MethodGet
			case uint16(wire.Flush):
				if _, err := fixture.adapter.WriteAt(ctx, open.Handle, data, 0); err != nil {
					t.Fatal(err)
				}
				request = flushMessage(t, session, session.NextMessageID, wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, 0)
			}
			if err := proxy.SetFault(s3fault.Fault{Method: method, Status: http.StatusServiceUnavailable}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := proxy.SetFault(s3fault.Fault{}); err != nil {
					t.Error(err)
				}
			})
			start := time.Now()
			if err := client.Send(ctx, []wire.Message{request}); err != nil {
				t.Fatal(err)
			}
			interim := receivePendingIO(ctx, t, client)
			assertInterimIO(t, request, interim)
			final := receivePendingIO(ctx, t, client)
			assertFinalIO(t, interim, final, smb.StatusIODeviceError)
			if command != wire.Read && (fixture.store.puts.Load() < 2 || time.Since(start) < time.Second) {
				t.Fatal("error did not exhaust the short upload retry budget")
			}
			if _, err := wire.DecodeErrorResponse(final); err != nil {
				t.Fatal(err)
			}
			response := ioRoundTrip(ctx, t, client, sessionEcho(t, session, session.NextMessageID+1))
			if response.Header.Status != smb.StatusSuccess {
				t.Fatal(response.Header)
			}
		})
	}
}

func TestS3OutagePendingIO(t *testing.T) {
	outage := 3 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = 300 * time.Second
	}
	for _, command := range []wire.Command{wire.Read, wire.Write, wire.Flush} {
		t.Run(commandName(command), func(t *testing.T) {
			fixture, proxy := newPendingIOFixture(t, true)
			server, client, ctx, session := pendingClient(t, fixture, outage+3*time.Minute)
			open := insertIOOpen(t, server, session, "outage", 3)
			cached := insertIOOpen(t, server, session, "cached", 3)
			cacheData := []byte("cached during outage")
			seedPendingData(ctx, t, fixture, cached, cacheData)
			warmPendingRead(ctx, t, fixture, cached, cacheData)
			data := bytes.Repeat([]byte("block boundary payload\n"), 6000)
			request := pendingWrite(t, session, session.NextMessageID, open, data[:32])
			want := data[:32]
			switch uint16(command) {
			case uint16(wire.Read):
				seedPendingData(ctx, t, fixture, open, data)
				if fixture.config.Chunk.BlockSize != 64<<10 || fixture.config.Chunk.CacheSize != 0 {
					t.Fatal("cold-cache block-boundary fixture changed")
				}
				const offset = (64 << 10) - 16
				request = pendingRead(t, session, session.NextMessageID, open, offset, 32)
				want = data[offset : offset+32]
			case uint16(wire.Flush):
				if _, err := fixture.adapter.WriteAt(ctx, open.Handle, want, 0); err != nil {
					t.Fatal(err)
				}
				request = flushMessage(t, session, session.NextMessageID, wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, 0)
			}
			t.Cleanup(proxy.RestoreS3)
			start := proxy.FailS3For(outage)
			if err := client.Send(ctx, []wire.Message{request}); err != nil {
				t.Fatal(err)
			}
			interim := receivePendingIO(ctx, t, client)
			assertInterimIO(t, request, interim)
			assertOutageStarted(t, proxy, start, command)
			assertUnrelatedIO(ctx, t, client, session, cached, session.NextMessageID+1, cacheData)
			final := receivePendingIO(ctx, t, client)
			assertFinalIO(t, interim, final, smb.StatusSuccess)
			if time.Since(start) < outage {
				t.Fatal("cold I/O completed during the outage")
			}
			assertIOSuccess(t, command, final, want)
			t.Logf("%s survived %s S3 outage, completed after %s", commandName(command), outage, time.Since(start))
		})
	}
}
