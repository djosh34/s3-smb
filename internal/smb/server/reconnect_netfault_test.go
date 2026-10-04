package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The gate holds exactly one request at the real adapter boundary. Cutting its
// transport cancels that request, without guessing when TCP traffic arrived.
type reconnectGate struct {
	entered  chan struct{}
	canceled chan struct{}
	command  wire.Command
}

type reconnectStorage struct {
	smb.Storage
	gate *reconnectGate
	mu   sync.Mutex
}

func (storage *reconnectStorage) arm(command wire.Command) *reconnectGate {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	gate := &reconnectGate{command: command, entered: make(chan struct{}), canceled: make(chan struct{})}
	storage.gate = gate
	return gate
}

func (storage *reconnectStorage) wait(ctx context.Context, command wire.Command) error {
	storage.mu.Lock()
	gate := storage.gate
	if gate == nil || gate.command != command {
		storage.mu.Unlock()
		return nil
	}
	storage.gate = nil
	storage.mu.Unlock()
	close(gate.entered)
	<-ctx.Done()
	close(gate.canceled)
	return ctx.Err()
}

func (storage *reconnectStorage) ReadAt(ctx context.Context, handle smb.Handle, dst []byte, offset uint64) (int, error) {
	if err := storage.wait(ctx, wire.Read); err != nil {
		return 0, err
	}
	return storage.Storage.ReadAt(ctx, handle, dst, offset)
}

func (storage *reconnectStorage) WriteAt(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
	if err := storage.wait(ctx, wire.Write); err != nil {
		return 0, err
	}
	return storage.Storage.WriteAt(ctx, handle, src, offset)
}

func (storage *reconnectStorage) Flush(ctx context.Context, handle smb.Handle, mode smb.SyncMode) error {
	if err := storage.wait(ctx, wire.Flush); err != nil {
		return err
	}
	return storage.Storage.Flush(ctx, handle, mode)
}

type reconnectClock struct {
	value time.Time
	mu    sync.Mutex
}

func (clock *reconnectClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.value
}

func (clock *reconnectClock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.value = clock.value.Add(duration)
}

type reconnectFixture struct {
	server  *Server
	storage *reconnectStorage
	clock   *reconnectClock
	ctx     context.Context
	login   smbtest.LoginOptions
}

func newReconnectFixture(t *testing.T, cipher uint16) *reconnectFixture {
	t.Helper()
	clock := &reconnectClock{value: time.Now()}
	options := testOptions(t)
	options.Now = clock.now
	storage := &reconnectStorage{Storage: newFilesMetaStorage(t)}
	options.Storage = storage
	var err error
	options.State, err = state.New(clock.now)
	if err != nil {
		t.Fatal(err)
	}
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Second)
		defer cleanupCancel()
		if err := server.Shutdown(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	return &reconnectFixture{
		server: server, storage: storage, clock: clock, ctx: ctx,
		login: smbtest.LoginOptions{Account: options.Account, Share: options.ShareName, ClientGUID: [16]byte{73}, Cipher: cipher, Signing: smb.SigningGMAC},
	}
}

type reconnectTransport struct {
	conn  net.Conn
	proxy *netfault.Proxy
	done  chan struct{}
}

// Each transport gets its own proxy, so cutting one client cannot cut the peer
// used to check detached sharing, ranges or a pending lease break.
func (fixture *reconnectFixture) transport(t *testing.T) *reconnectTransport {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(fixture.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
	})
	proxy, err := netfault.New(fixture.ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &reconnectTransport{proxy: proxy, done: make(chan struct{})}
	go func() {
		defer close(transport.done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if !errors.Is(acceptErr, net.ErrClosed) {
				t.Error(acceptErr)
			}
			return
		}
		if serveErr := fixture.server.ServeConn(fixture.ctx, conn); serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			t.Error(serveErr)
		}
	}()
	var dialer net.Dialer
	transport.conn, err = dialer.DialContext(fixture.ctx, "tcp", proxy.Address())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(fixture.ctx), 3*time.Second)
		defer cancel()
		awaitReconnectEvent(cleanupCtx, t, transport.done)
	})
	return transport
}

func awaitReconnectEvent(ctx context.Context, t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func (fixture *reconnectFixture) client(t *testing.T, guid [16]byte) (*smbtest.Client, smbtest.Session, *reconnectTransport) {
	t.Helper()
	transport := fixture.transport(t)
	client, err := smbtest.NewClient(transport.conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	options := fixture.login
	options.ClientGUID = guid
	session, err := client.Login(fixture.ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	return client, session, transport
}

func reconnectIO(t *testing.T, session *smbtest.Session, command wire.Command, id wire.FileID, data []byte, offset uint64) wire.Message {
	t.Helper()
	var body []byte
	var err error
	length := len(data)
	if length > 65536 {
		t.Fatal("reconnect test I/O exceeds one credit")
		return wire.Message{}
	}
	switch uint16(command) {
	case uint16(wire.Write):
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data, Offset: offset})
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: uint32(length), Offset: offset})
	case uint16(wire.Flush):
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{ID: id})
	default:
		t.Fatalf("unsupported reconnect I/O command %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(*session, session.NextMessageID, command, body, 1)
	session.NextMessageID++
	return message
}

func TestReconnectNetfaultFixture(t *testing.T) {
	fixture := newReconnectFixture(t, smb.CipherAES128GCM)
	client, session, transport := fixture.client(t, fixture.login.ClientGUID)
	open := insertIOOpen(t, fixture.server, session, "band", fileReadData|fileWriteData)
	id := wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}
	for _, command := range []wire.Command{wire.Write, wire.Read, wire.Flush} {
		message := reconnectIO(t, &session, command, id, []byte("acknowledged"), 0)
		if reply := ioRoundTrip(fixture.ctx, t, client, message); reply.Header.Status != smb.StatusSuccess {
			t.Fatal(reply.Header)
		}
	}
	gate := fixture.storage.arm(wire.Write)
	message := reconnectIO(t, &session, wire.Write, id, []byte("unanswered"), 0)
	if err := client.Send(fixture.ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	awaitReconnectEvent(fixture.ctx, t, gate.entered)
	if err := transport.proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	awaitReconnectEvent(fixture.ctx, t, gate.canceled)
	awaitReconnectEvent(fixture.ctx, t, transport.done)
	fixture.clock.advance(30 * time.Second)
}
