package server

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Use the shared file-backed fixture behind a real S3 client and fault proxy.
// GET uses FileServer's range support; PUT stores the bytes in the same backend.
func newPendingIOFixture(t *testing.T, outage bool) (*ioFixture, *s3fault.Proxy) {
	t.Helper()
	var proxy *s3fault.Proxy
	window := 100 * time.Millisecond
	if outage {
		window = 0
	}
	fixture := newConfiguredIOFixture(t, nil, func(dir string, mc *meta.Config, cc *chunk.Config, store *ioStore) {
		backendStore := store.ObjectStorage
		files := http.StripPrefix("/bucket/", http.FileServer(http.Dir(filepath.Join(dir, "objects"))))
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				files.ServeHTTP(w, r)
				return
			}
			key := strings.TrimPrefix(r.URL.Path, "/bucket/")
			if err := backendStore.Put(r.Context(), key, r.Body); err != nil {
				t.Errorf("S3 fixture PUT: %v", err)
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(backend.Close)
		var err error
		proxy, err = s3fault.New(t.Context(), backend.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := proxy.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		store.ObjectStorage, err = object.CreateStorage("s3", proxy.URL()+"/bucket", "test-access", "test-secret", "")
		if err != nil {
			t.Fatal(err)
		}
		mc.Retries = 2
		if outage {
			mc.Retries, cc.MaxRetries = 4, 3
			if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
				// Match the production retry budgets and block timeouts in
				// internal/storage/runtime.go, but keep the cache cold.
				mc.Retries, cc.MaxRetries = 53, 12
				cc.GetTimeout, cc.PutTimeout = time.Minute, time.Minute
				store.put = func(ctx context.Context, key string, src io.Reader) error {
					start := time.Now()
					putErr := store.ObjectStorage.Put(ctx, key, src)
					t.Logf("S3 PUT %s at %s: duration %s, error %v", key, start.Format(time.RFC3339Nano), time.Since(start), putErr)
					return putErr
				}
			}
		}
	}, window)
	return fixture, proxy
}

func pendingClient(t *testing.T, fixture *ioFixture, bound time.Duration) (*Server, *smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := boundedPipeClient(t, server, false, bound)
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return server, client, ctx, session
}

func pendingRead(t *testing.T, session smbtest.Session, id uint64, open state.Open, offset uint64, length uint32) wire.Message {
	t.Helper()
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, Offset: offset, Length: length})
	if err != nil {
		t.Fatal(err)
	}
	charge := pendingCharge(t, length)
	return ioMessage(session, id, wire.Read, body, charge)
}

func pendingWrite(t *testing.T, session smbtest.Session, id uint64, open state.Open, data []byte) wire.Message {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, Data: data, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	charge := pendingCharge(t, pendingLength(t, data))
	return ioMessage(session, id, wire.Write, body, charge)
}

func pendingLength(t *testing.T, data []byte) uint32 {
	t.Helper()
	length := len(data)
	if length < 0 || length > math.MaxUint32 {
		t.Fatal("I/O test payload is too large")
		return 0
	}
	return uint32(length)
}

func pendingCharge(t *testing.T, length uint32) uint16 {
	t.Helper()
	charge := (uint64(length) + uint64(smb.CreditUnit) - 1) / uint64(smb.CreditUnit)
	if charge > 16 {
		t.Fatal("I/O test payload is too large")
		return 0
	}
	return uint16(charge)
}

func receivePendingIO(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply: %+v", response.Messages)
	}
	return response.Messages[0]
}

func assertInterimIO(t *testing.T, request, interim wire.Message) {
	t.Helper()
	if interim.Header.Status != smb.StatusPending || interim.Header.Flags&wire.FlagAsync == 0 || interim.Header.AsyncID == 0 || interim.Header.MessageID != request.Header.MessageID || interim.Header.SessionID != request.Header.SessionID || interim.Header.Command != request.Header.Command || interim.Header.Credit != request.Header.Credit {
		t.Fatalf("invalid interim reply: %+v for %+v", interim.Header, request.Header)
	}
	if _, err := wire.DecodeErrorResponse(interim); err != nil {
		t.Fatal(err)
	}
}

func assertFinalIO(t *testing.T, interim, final wire.Message, status smb.Status) {
	t.Helper()
	if final.Header.Status != status || final.Header.Flags&wire.FlagAsync == 0 || final.Header.AsyncID != interim.Header.AsyncID || final.Header.MessageID != interim.Header.MessageID || final.Header.SessionID != interim.Header.SessionID || final.Header.Command != interim.Header.Command || final.Header.Credit != 0 {
		t.Fatalf("invalid final reply: %+v after %+v", final.Header, interim.Header)
	}
}

func seedPendingData(ctx context.Context, t *testing.T, fixture *ioFixture, open state.Open, data []byte) {
	t.Helper()
	if n, err := fixture.adapter.WriteAt(ctx, open.Handle, data, 0); err != nil || n != len(data) {
		t.Fatalf("seed write: %d, %v", n, err)
	}
	if err := fixture.adapter.Flush(ctx, open.Handle, smb.SyncData); err != nil {
		t.Fatal(err)
	}
}

func warmPendingRead(ctx context.Context, t *testing.T, fixture *ioFixture, open state.Open, data []byte) {
	t.Helper()
	got := make([]byte, len(data))
	if n, err := fixture.adapter.ReadAt(ctx, open.Handle, got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("warm read: %d, %q, %v", n, got, err)
	}
}

func assertUnrelatedIO(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, cached state.Open, next uint64, data []byte) {
	t.Helper()
	// Both replies must arrive promptly while the storage request is pending.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	echoRequest := sessionEcho(t, session, next)
	response := ioRoundTrip(ctx, t, client, echoRequest)
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	response = ioRoundTrip(ctx, t, client, pendingRead(t, session, next+1, cached, 0, pendingLength(t, data)))
	assertReadIO(t, response, data)
}

func assertReadIO(t *testing.T, response wire.Message, data []byte) {
	t.Helper()
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	result, err := wire.DecodeReadResponse(response)
	if err != nil || !bytes.Equal(result.Data, data) {
		t.Fatalf("read data: %q, %v", result.Data, err)
	}
}

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

func assertIOSuccess(t *testing.T, command wire.Command, final wire.Message, data []byte) {
	t.Helper()
	switch uint16(command) {
	case uint16(wire.Read):
		assertReadIO(t, final, data)
	case uint16(wire.Write):
		result, err := wire.DecodeWriteResponse(final)
		if err != nil || int(result.Count) != len(data) {
			t.Fatalf("WRITE result: %+v, %v", result, err)
		}
	case uint16(wire.Flush):
		if _, err := wire.DecodeFlushResponse(final); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unexpected I/O command")
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

func assertOutageStarted(t *testing.T, proxy *s3fault.Proxy, start time.Time, command wire.Command) {
	t.Helper()
	paths := make(map[string]bool)
	count, method := 1, http.MethodPut
	if command == wire.Read {
		count, method = 2, http.MethodGet
	}
	timer := time.NewTimer(time.Until(start.Add(time.Second)))
	defer timer.Stop()
	for len(paths) < count {
		select {
		case event := <-proxy.OutageSeen():
			if event.Method != method || event.Status != http.StatusServiceUnavailable || time.Since(start) >= time.Second {
				t.Fatalf("unexpected outage request: %+v after %s", event, time.Since(start))
			}
			paths[event.Path] = true
		case <-timer.C:
			t.Fatal("cold I/O did not reach failed S3 in the first second")
		}
	}
}
