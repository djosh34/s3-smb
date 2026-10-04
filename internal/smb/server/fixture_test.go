package server

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// This file is the shared fixture for server tests. newTestServer serves a
// real JuiceFS adapter over loopback TCP and connect logs a client in. Tests
// speak SMB through the client's per-command methods, make storage fail,
// pause or block through server.faults, and move time with server.clock.
//
//	srv := newTestServer(t)
//	client := srv.connect(t)
//	id := client.open(t, "file")
//	if status := client.write(t, wire.WriteRequest{ID: id, Data: []byte("data")}); status != smb.StatusSuccess {
//		t.Fatal(status)
//	}

// testServer is one SMB server on real storage. Its fault storage sits between
// the server and adapter. White-box checks may use server.options.State.
type testServer struct {
	listener net.Listener
	server   *Server
	adapter  *smbfs.FS
	faults   *faultStorage
	clock    *fakeClock
}

// newTestServer starts a server on a fresh file-backed JuiceFS runtime.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerOn(t, smbtest.NewStorage(t))
}

// newTestServerOn starts a server on adapter, such as one from
// smbtest.NewS3Storage. It accepts signed and encrypted sessions for the
// account backup/password on share backup. Cleanup shuts the server down
// before the adapter's own cleanup runs.
func newTestServerOn(t *testing.T, adapter *smbfs.FS) *testServer {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	table, err := state.New(clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	faults := &faultStorage{Storage: adapter}
	server, err := New(Options{
		Storage: faults, State: table, Logger: slog.New(slog.DiscardHandler), Now: clock.Now,
		Account: auth.Account{User: "backup", Password: "password"}, ShareName: "backup",
		ServerName: "s3-smb", ServerGUID: [16]byte{1}, Encryption: AllowPlaintext,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(listener.Close(), server.Shutdown(context.WithoutCancel(t.Context()))); err != nil {
			t.Error(err)
		}
	})
	return &testServer{listener: listener, server: server, adapter: adapter, faults: faults, clock: clock}
}

// expire runs one expiry pass at the clock's time and waits for its cleanup.
// The live scavenger reads the same clock, so it never expires anything early.
func (s *testServer) expire(t *testing.T) {
	s.server.expire(t.Context())
	s.server.scavengerCleanup.Wait()
}

// connect logs a new client in with AES-128-GCM and a random client GUID.
func (s *testServer) connect(t *testing.T) *testClient {
	t.Helper()
	return s.dial(t, smbtest.LoginOptions{Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC})
}

// dial logs a new client in over its own TCP connection. Share and Account
// are filled in; Cipher 0 gives a signed plaintext session and a zero
// ClientGUID a random one. Dial from the test goroutine only.
func (s *testServer) dial(t *testing.T, login smbtest.LoginOptions) *testClient {
	t.Helper()
	login.Share, login.Account = s.server.options.ShareName, s.server.options.Account
	client := s.accept(t)
	client.login = login
	var err error
	client.session, err = client.raw.Login(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// accept opens a new TCP connection to the server without logging in. Its
// requests go out unprotected, starting at message ID 0. Cleanup closes the
// client and reports any error from the server side of the connection; a test
// that expects the server to end the connection reads that error with ended.
func (s *testServer) accept(t *testing.T) *testClient {
	t.Helper()
	var dialer net.Dialer
	dialed, err := dialer.DialContext(t.Context(), "tcp", s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, ok := dialed.(*net.TCPConn)
	if !ok {
		t.Fatalf("dialed %T, not TCP", dialed)
	}
	accepted, err := s.listener.Accept()
	if err != nil {
		t.Fatal(errors.Join(err, conn.Close()))
	}
	served := make(chan error, 1)
	go func() { served <- s.server.ServeConn(context.WithoutCancel(t.Context()), accepted) }()
	raw, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{server: s, raw: raw, conn: conn, served: served, replies: make(map[uint64][]wire.Message), interims: make(map[uint64]wire.Header)}
	t.Cleanup(func() {
		if closeErr := errors.Join(client.disconnect(), raw.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

// testClient is one logged-in SMB session with its tree connected. Its
// methods send one request and return the final reply's status with the
// decoded response, failing the test on transport or decoding errors. Use
// send, interim and receive for requests that should go async, and raw for
// anything the methods do not cover. Only the test goroutine may use it.
type testClient struct {
	conn     *net.TCPConn
	server   *testServer
	raw      *smbtest.Client
	served   chan error
	replies  map[uint64][]wire.Message
	interims map[uint64]wire.Header
	login    smbtest.LoginOptions
	session  smbtest.Session
}

// drop cuts the network under the client. The server sees the connection end
// with the client's requests still open; drop returns once the server has
// finished with the connection and detached its opens. Replies the server sent
// before that can still be read from raw.
func (c *testClient) drop(t *testing.T) {
	t.Helper()
	if err := c.disconnect(); err != nil {
		t.Fatal(err)
	}
}

// disconnect half-closes the connection, so the server reads EOF however many
// replies the client left unread, and returns the server's connection error.
func (c *testClient) disconnect() error {
	if c.served == nil {
		return nil
	}
	err := errors.Join(c.conn.CloseWrite(), <-c.served)
	c.served = nil
	return err
}

// ended waits for the server to end the connection on its own and returns
// the server's error.
func (c *testClient) ended() error {
	err := <-c.served
	c.served = nil
	return err
}

// reconnect logs in on a new connection as this client coming back after a
// drop: same client GUID, account and algorithms, with PreviousSessionID set.
// A create with a Reconnect context then reclaims a durable handle.
func (c *testClient) reconnect(t *testing.T) *testClient {
	t.Helper()
	login := c.login
	login.ClientGUID, login.PreviousSessionID = c.session.ClientGUID, c.session.SessionID
	return c.server.dial(t, login)
}

// header returns a request header with the next message IDs. A multi-credit
// request uses charge consecutive IDs.
func (c *testClient) header(command wire.Command, charge uint16) wire.Header {
	header := wire.Header{Command: command, MessageID: c.session.NextMessageID, SessionID: c.session.SessionID, TreeID: c.session.TreeID, CreditCharge: charge, Credit: 32}
	c.session.NextMessageID += uint64(charge)
	return header
}

// send sends one request without waiting and returns its header for interim
// and receive. charge is creditsFor(n) for a READ or WRITE of n bytes, else 1.
func (c *testClient) send(t *testing.T, command wire.Command, body []byte, charge uint16) wire.Header {
	t.Helper()
	header := c.header(command, charge)
	if err := c.raw.Send(t.Context(), []wire.Message{{Header: header, Body: body}}); err != nil {
		t.Fatal(err)
	}
	return header
}

// interim receives the interim reply to request and checks that it is
// STATUS_PENDING with an async ID. receive then checks the final reply.
func (c *testClient) interim(t *testing.T, request wire.Header) wire.Header {
	t.Helper()
	header := c.next(t, request).Header
	c.pending(t, header)
	return header
}

// receive returns the final reply to request. It checks and skips an interim
// reply, and a final reply after one must keep its async ID and grant no
// credits. Replies to other requests are kept for their own receive calls.
func (c *testClient) receive(t *testing.T, request wire.Header) wire.Message {
	t.Helper()
	message := c.next(t, request)
	if message.Header.Status == smb.StatusPending {
		c.pending(t, message.Header)
		message = c.next(t, request)
	}
	if interim, async := c.interims[request.MessageID]; async {
		delete(c.interims, request.MessageID)
		if final := message.Header; final.Flags&wire.FlagAsync == 0 || final.AsyncID != interim.AsyncID || final.Credit != 0 {
			t.Fatalf("final reply %+v does not match interim reply %+v", final, interim)
		}
	}
	return message
}

func (c *testClient) pending(t *testing.T, header wire.Header) {
	t.Helper()
	if header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 {
		t.Fatalf("not an interim reply: %+v", header)
	}
	c.interims[header.MessageID] = header
}

func (c *testClient) next(t *testing.T, request wire.Header) wire.Message {
	t.Helper()
	for len(c.replies[request.MessageID]) == 0 {
		reply, err := c.raw.Receive(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range reply.Messages {
			id := message.Header.MessageID
			c.replies[id] = append(c.replies[id], message)
		}
	}
	message := c.replies[request.MessageID][0]
	c.replies[request.MessageID] = c.replies[request.MessageID][1:]
	if message.Header.Command != request.Command {
		t.Fatalf("reply %+v answers another command than %+v", message.Header, request)
	}
	return message
}

func (c *testClient) call(t *testing.T, command wire.Command, body []byte, charge uint16) wire.Message {
	t.Helper()
	return c.receive(t, c.send(t, command, body, charge))
}

// sendCreate sends CREATE with the typed lease and durable contexts in
// options. Use created to read its reply.
func (c *testClient) sendCreate(t *testing.T, options smbtest.CreateOptions) wire.Header {
	t.Helper()
	header := c.header(wire.Create, 1)
	if err := c.raw.SendCreate(t.Context(), header, options); err != nil {
		t.Fatal(err)
	}
	return header
}

// created returns the decoded final reply to a CREATE.
func (c *testClient) created(t *testing.T, request wire.Header) (smbtest.CreateResult, smb.Status) {
	t.Helper()
	return decodeReply(t, c.receive(t, request), smbtest.DecodeCreateReply)
}

// create sends CREATE and returns its decoded reply with any lease and durable
// grants.
func (c *testClient) create(t *testing.T, options smbtest.CreateOptions) (smbtest.CreateResult, smb.Status) {
	t.Helper()
	return c.created(t, c.sendCreate(t, options))
}

// open opens or creates the file name with full access, sharing everything,
// and fails the test unless that succeeds.
func (c *testClient) open(t *testing.T, name string) wire.FileID {
	t.Helper()
	result, status := c.create(t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}})
	if status != smb.StatusSuccess {
		t.Fatalf("open %q: status %#x", name, status)
	}
	return result.Reply.ID
}

func (c *testClient) close(t *testing.T, id wire.FileID) smb.Status {
	t.Helper()
	return c.call(t, wire.Close, encode(t, wire.EncodeCloseRequest, wire.CloseRequest{ID: id}), 1).Header.Status
}

func (c *testClient) read(t *testing.T, request wire.ReadRequest) ([]byte, smb.Status) {
	t.Helper()
	response, status := decodeReply(t, c.call(t, wire.Read, encode(t, wire.EncodeReadRequest, request), creditsFor(int(request.Length))), wire.DecodeReadResponse)
	return response.Data, status
}

// write fails the test if a successful WRITE reports a short count.
func (c *testClient) write(t *testing.T, request wire.WriteRequest) smb.Status {
	t.Helper()
	response, status := decodeReply(t, c.call(t, wire.Write, encode(t, wire.EncodeWriteRequest, request), creditsFor(len(request.Data))), wire.DecodeWriteResponse)
	if status == smb.StatusSuccess && int(response.Count) != len(request.Data) {
		t.Fatalf("WRITE count %d, want %d", response.Count, len(request.Data))
	}
	return status
}

func (c *testClient) flush(t *testing.T, request wire.FlushRequest) smb.Status {
	t.Helper()
	return c.call(t, wire.Flush, encode(t, wire.EncodeFlushRequest, request), 1).Header.Status
}

// queryInfo returns the output buffer, which STATUS_BUFFER_OVERFLOW truncates.
func (c *testClient) queryInfo(t *testing.T, request wire.QueryInfoRequest) ([]byte, smb.Status) {
	t.Helper()
	response, status := decodeReply(t, c.call(t, wire.QueryInfo, encode(t, wire.EncodeQueryInfoRequest, request), 1), wire.DecodeQueryInfoResponse)
	return response.Data, status
}

func (c *testClient) setInfo(t *testing.T, request wire.SetInfoRequest) smb.Status {
	t.Helper()
	return c.call(t, wire.SetInfo, encode(t, wire.EncodeSetInfoRequest, request), 1).Header.Status
}

func (c *testClient) queryDirectory(t *testing.T, request wire.QueryDirectoryRequest) ([]byte, smb.Status) {
	t.Helper()
	response, status := decodeReply(t, c.call(t, wire.QueryDirectory, encode(t, wire.EncodeQueryDirectoryRequest, request), 1), wire.DecodeQueryDirectoryResponse)
	return response.Data, status
}

func (c *testClient) lock(t *testing.T, request wire.LockRequest) smb.Status {
	t.Helper()
	return c.call(t, wire.Lock, encode(t, wire.EncodeLockRequest, request), 1).Header.Status
}

// ioctl returns only a status: the server refuses every control code.
func (c *testClient) ioctl(t *testing.T, request wire.IOCTLRequest) smb.Status {
	t.Helper()
	return c.call(t, wire.IOCTL, encode(t, wire.EncodeIOCTLRequest, request), 1).Header.Status
}

// echo fails the test unless ECHO succeeds, proving the connection still works.
func (c *testClient) echo(t *testing.T) {
	t.Helper()
	if status := c.call(t, wire.Echo, encode(t, wire.EncodeEchoRequest, wire.EmptyRequest{}), 1).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("ECHO status %#x", status)
	}
}

// cancelAsync sends CANCEL for the request behind an interim reply.
func (c *testClient) cancelAsync(t *testing.T, interim wire.Header) {
	t.Helper()
	header := wire.Header{Command: wire.Cancel, Flags: wire.FlagAsync, AsyncID: interim.AsyncID, SessionID: c.session.SessionID}
	if err := c.raw.Send(t.Context(), []wire.Message{{Header: header, Body: encode(t, wire.EncodeCancelRequest, wire.EmptyRequest{})}}); err != nil {
		t.Fatal(err)
	}
}

// sendUnprotected sends messages as one compound without signing or
// encrypting it, whatever the session requires.
func (c *testClient) sendUnprotected(t *testing.T, messages ...wire.Message) {
	t.Helper()
	if err := c.raw.SendRaw(t.Context(), frame(t, messages...)); err != nil {
		t.Fatal(err)
	}
}

// noExtraReplies fails the test if the server sent a reply that no receive
// call took, such as a reply to CANCEL or a second final reply.
func (c *testClient) noExtraReplies(t *testing.T) {
	t.Helper()
	c.echo(t)
	for messageID, replies := range c.replies {
		if len(replies) != 0 {
			t.Fatalf("message %d answered again: %+v", messageID, replies)
		}
	}
}

// leaseBreak waits for the next lease break notification to this client.
func (c *testClient) leaseBreak(t *testing.T) wire.LeaseBreakNotification {
	t.Helper()
	notification, err := c.raw.WaitLeaseBreak(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return notification
}

// ackLease acknowledges a lease break, keeping leaseState. A successful reply
// must echo the key and state.
func (c *testClient) ackLease(t *testing.T, key [16]byte, leaseState uint32) smb.Status {
	t.Helper()
	request := wire.LeaseBreakRequest{Key: key, State: leaseState}
	response, status := decodeReply(t, c.call(t, wire.OplockBreak, encode(t, wire.EncodeLeaseBreakRequest, request), 1), wire.DecodeLeaseBreakResponse)
	if status == smb.StatusSuccess && (response.Key != key || response.State != leaseState) {
		t.Fatalf("lease ACK reply %+v, want key %x state %#x", response, key, leaseState)
	}
	return status
}

// creditsFor returns the credits a READ or WRITE of size bytes costs.
func creditsFor(size int) uint16 {
	unit := int(smb.CreditUnit)
	return uint16(min(max(1, (size+unit-1)/unit), math.MaxUint16))
}

func encode[T any](t testing.TB, encoder func(T) ([]byte, error), request T) []byte {
	t.Helper()
	body, err := encoder(request)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// frame joins messages into one direct TCP frame, unprotected.
func frame(t testing.TB, messages ...wire.Message) []byte {
	t.Helper()
	payload, err := wire.Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(payload)&0xffffff)), payload...)
}

// negotiateRequest is the NEGOTIATE a Mac sends: SMB 3.1.1 with SHA-512
// preauth, both GCM ciphers and both signing algorithms.
func negotiateRequest(t testing.TB) []byte {
	t.Helper()
	contexts := make([]wire.NegotiateContext, 3)
	var errs [3]error
	contexts[0], errs[0] = wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}, Salt: make([]byte, 32)})
	contexts[1], errs[1] = wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM}})
	contexts[2], errs[2] = wire.EncodeSigningContext(wire.SigningContext{Algorithms: []uint16{smb.SigningCMAC, smb.SigningGMAC}})
	if err := errors.Join(errs[:]...); err != nil {
		t.Fatal(err)
	}
	return encode(t, wire.EncodeNegotiateRequest, wire.NegotiateRequest{
		Dialects: []uint16{smb.Dialect311}, SecurityMode: smb.AdvertisedSecurityMode, Contexts: contexts, ClientGUID: [16]byte{2},
	})
}

// loginStart is the body of the first SESSION_SETUP of an NTLMv2 login.
func loginStart(t testing.TB) []byte {
	t.Helper()
	account := auth.Account{User: "backup", Password: "password"}
	acceptor, err := auth.NewAcceptor(auth.Options{Account: account, ServerName: "s3-smb", Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	token, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	initiator, err := auth.NewInitiator(account, nil)
	if err != nil {
		t.Fatal(err)
	}
	start, err := initiator.Start(token)
	if err != nil {
		t.Fatal(err)
	}
	return encode(t, wire.EncodeSessionSetupRequest, wire.SessionSetupRequest{Token: start.Token, SecurityMode: 3})
}

// message builds a one-credit request with the client's next message ID.
func message[T any](t *testing.T, client *testClient, command wire.Command, encoder func(T) ([]byte, error), request T) wire.Message {
	t.Helper()
	return wire.Message{Header: client.header(command, 1), Body: encode(t, encoder, request)}
}

func echoMessage(t *testing.T, client *testClient) wire.Message {
	t.Helper()
	return message(t, client, wire.Echo, wire.EncodeEchoRequest, wire.EmptyRequest{})
}

// finalStatuses returns the status of the final reply to each message.
func finalStatuses(t *testing.T, client *testClient, messages []wire.Message) []smb.Status {
	t.Helper()
	statuses := make([]smb.Status, len(messages))
	for index, message := range messages {
		statuses[index] = client.receive(t, message.Header).Header.Status
	}
	return statuses
}

// sendCompound sends messages as one compound and returns their final statuses.
func sendCompound(t *testing.T, client *testClient, messages ...wire.Message) []smb.Status {
	t.Helper()
	if err := client.raw.Send(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	return finalStatuses(t, client, messages)
}

// placeholder is the file ID by which a related compound member names the
// file of the member before it.
var placeholder = wire.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}

// related makes message a related member of the compound it is sent in.
func related(message wire.Message) wire.Message {
	message.Header.Flags |= wire.FlagRelated
	message.Header.SessionID, message.Header.TreeID = math.MaxUint64, math.MaxUint32
	return message
}

// decodeReply decodes a reply that carries a response body: success, or
// STATUS_BUFFER_OVERFLOW with truncated output. Other statuses decode nothing.
func decodeReply[T any](t *testing.T, reply wire.Message, decoder func(wire.Message) (T, error)) (T, smb.Status) {
	t.Helper()
	var response T
	status := reply.Header.Status
	if status != smb.StatusSuccess && status != smb.StatusBufferOverflow {
		return response, status
	}
	response, err := decoder(reply)
	if err != nil {
		t.Fatal(err)
	}
	return response, status
}

// fakeClock is the server's clock. It starts at the wall-clock time, so logins
// work, and moves only when a test calls advance.
type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// holdWrites makes storage WriteAt wait until the test closes release or the
// request is cancelled. Each call first signals on entered.
func (s *testServer) holdWrites() (entered <-chan struct{}, release chan<- struct{}) {
	enteredCh, releaseCh := make(chan struct{}), make(chan struct{})
	s.faults.set(func(hooks *storageHooks) {
		hooks.WriteAt = func(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
			select {
			case enteredCh <- struct{}{}:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			select {
			case <-releaseCh:
				return s.adapter.WriteAt(ctx, handle, src, offset)
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
	})
	return enteredCh, releaseCh
}

// faultStorage passes every call to the real adapter unless the test sets a
// hook for that method with set. A hook replaces the call: it can return an
// error, block on a channel, or call srv.adapter itself before or after
// waiting. A blocking hook should also return when ctx ends, so a failing
// test does not hang in cleanup. Hooks may change while the server runs.
type faultStorage struct {
	smb.Storage
	hooks storageHooks
	mu    sync.Mutex
}

// storageHooks has one optional hook per smb.Storage method; nil passes through.
type storageHooks struct {
	Lookup   func(ctx context.Context, path string) (smb.Resolved, error)
	Open     func(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error)
	Create   func(ctx context.Context, name smb.Name, kind smb.Kind) (smb.Resolved, error)
	Close    func(ctx context.Context, handle smb.Handle) error
	ReadAt   func(ctx context.Context, handle smb.Handle, dst []byte, offset uint64) (int, error)
	WriteAt  func(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error)
	Flush    func(ctx context.Context, handle smb.Handle, mode smb.SyncMode) error
	Truncate func(ctx context.Context, handle smb.Handle, size uint64) error
	GetAttr  func(ctx context.Context, object smb.ObjectKey) (smb.Attr, error)
	SetAttr  func(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error
	ReadDir  func(ctx context.Context, inode smb.Inode, cookie smb.Cookie, limit uint32) ([]smb.DirEntry, error)
	Streams  func(ctx context.Context, inode smb.Inode) ([]smb.StreamInfo, error)
	Remove   func(ctx context.Context, name smb.Name, expect smb.Inode) error
	Rename   func(ctx context.Context, request smb.RenameRequest) error
	PathOf   func(ctx context.Context, inode smb.Inode) (string, error)
	StatFS   func(ctx context.Context) (smb.Space, error)
}

// set changes hooks under the storage lock, for example:
//
//	srv.faults.set(func(h *storageHooks) { h.Flush = nil })
func (f *faultStorage) set(change func(*storageHooks)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.hooks)
}

func (f *faultStorage) current() storageHooks {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hooks
}

func (f *faultStorage) Lookup(ctx context.Context, path string) (smb.Resolved, error) {
	if hook := f.current().Lookup; hook != nil {
		return hook(ctx, path)
	}
	return f.Storage.Lookup(ctx, path)
}

func (f *faultStorage) Open(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	if hook := f.current().Open; hook != nil {
		return hook(ctx, object, access)
	}
	return f.Storage.Open(ctx, object, access)
}

func (f *faultStorage) Create(ctx context.Context, name smb.Name, kind smb.Kind) (smb.Resolved, error) {
	if hook := f.current().Create; hook != nil {
		return hook(ctx, name, kind)
	}
	return f.Storage.Create(ctx, name, kind)
}

func (f *faultStorage) Close(ctx context.Context, handle smb.Handle) error {
	if hook := f.current().Close; hook != nil {
		return hook(ctx, handle)
	}
	return f.Storage.Close(ctx, handle)
}

func (f *faultStorage) ReadAt(ctx context.Context, handle smb.Handle, dst []byte, offset uint64) (int, error) {
	if hook := f.current().ReadAt; hook != nil {
		return hook(ctx, handle, dst, offset)
	}
	return f.Storage.ReadAt(ctx, handle, dst, offset)
}

func (f *faultStorage) WriteAt(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
	if hook := f.current().WriteAt; hook != nil {
		return hook(ctx, handle, src, offset)
	}
	return f.Storage.WriteAt(ctx, handle, src, offset)
}

func (f *faultStorage) Flush(ctx context.Context, handle smb.Handle, mode smb.SyncMode) error {
	if hook := f.current().Flush; hook != nil {
		return hook(ctx, handle, mode)
	}
	return f.Storage.Flush(ctx, handle, mode)
}

func (f *faultStorage) Truncate(ctx context.Context, handle smb.Handle, size uint64) error {
	if hook := f.current().Truncate; hook != nil {
		return hook(ctx, handle, size)
	}
	return f.Storage.Truncate(ctx, handle, size)
}

func (f *faultStorage) GetAttr(ctx context.Context, object smb.ObjectKey) (smb.Attr, error) {
	if hook := f.current().GetAttr; hook != nil {
		return hook(ctx, object)
	}
	return f.Storage.GetAttr(ctx, object)
}

func (f *faultStorage) SetAttr(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error {
	if hook := f.current().SetAttr; hook != nil {
		return hook(ctx, object, change)
	}
	return f.Storage.SetAttr(ctx, object, change)
}

func (f *faultStorage) ReadDir(ctx context.Context, inode smb.Inode, cookie smb.Cookie, limit uint32) ([]smb.DirEntry, error) {
	if hook := f.current().ReadDir; hook != nil {
		return hook(ctx, inode, cookie, limit)
	}
	return f.Storage.ReadDir(ctx, inode, cookie, limit)
}

func (f *faultStorage) Streams(ctx context.Context, inode smb.Inode) ([]smb.StreamInfo, error) {
	if hook := f.current().Streams; hook != nil {
		return hook(ctx, inode)
	}
	return f.Storage.Streams(ctx, inode)
}

func (f *faultStorage) Remove(ctx context.Context, name smb.Name, expect smb.Inode) error {
	if hook := f.current().Remove; hook != nil {
		return hook(ctx, name, expect)
	}
	return f.Storage.Remove(ctx, name, expect)
}

func (f *faultStorage) Rename(ctx context.Context, request smb.RenameRequest) error {
	if hook := f.current().Rename; hook != nil {
		return hook(ctx, request)
	}
	return f.Storage.Rename(ctx, request)
}

func (f *faultStorage) PathOf(ctx context.Context, inode smb.Inode) (string, error) {
	if hook := f.current().PathOf; hook != nil {
		return hook(ctx, inode)
	}
	return f.Storage.PathOf(ctx, inode)
}

func (f *faultStorage) StatFS(ctx context.Context) (smb.Space, error) {
	if hook := f.current().StatFS; hook != nil {
		return hook(ctx)
	}
	return f.Storage.StatFS(ctx)
}
