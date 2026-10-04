package server_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/smbtest/fixture"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type lifecycleClient struct {
	ctx     context.Context
	client  *smbtest.Client
	opens   *state.Table
	session smbtest.Session
	next    uint64
}

func newLifecycleClient(t *testing.T, storage smb.Storage) *lifecycleClient {
	t.Helper()
	opens, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	options := server.Options{
		Storage: storage, State: opens, Now: time.Now,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Account:   auth.Account{User: "backup", Password: "password"},
		ShareName: "backup", ServerName: "s3-smb", ServerGUID: [16]byte{1},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	running, err := fixture.Start(context.WithoutCancel(ctx), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if closeErr := running.Close(cleanup); closeErr != nil {
			t.Error(closeErr)
		}
	})
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", running.Address())
	if err != nil {
		t.Fatal(err)
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(errors.Join(err, conn.Close()))
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{
		Account: options.Account, Share: options.ShareName, ClientGUID: [16]byte{2},
		Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleClient{client: client, ctx: ctx, session: session, opens: opens, next: session.NextMessageID}
}

func (c *lifecycleClient) message(command wire.Command, body []byte, charge uint16) wire.Message {
	message := wire.Message{Header: wire.Header{
		Command: command, MessageID: c.next, SessionID: c.session.SessionID,
		TreeID: c.session.TreeID, CreditCharge: charge, Credit: 16,
	}, Body: body}
	c.next += uint64(charge)
	return message
}

func (c *lifecycleClient) receive(t *testing.T) wire.Message {
	t.Helper()
	reply, err := c.client.Receive(c.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Messages) != 1 {
		t.Fatalf("expected one reply, got %d", len(reply.Messages))
	}
	return reply.Messages[0]
}

func checkLifecycleInterim(t *testing.T, request, interim wire.Message) {
	t.Helper()
	h := interim.Header
	if h.Status != smb.StatusPending || h.Flags&wire.FlagAsync == 0 || h.AsyncID == 0 ||
		h.Command != request.Header.Command || h.MessageID != request.Header.MessageID ||
		h.SessionID != request.Header.SessionID || h.Credit != request.Header.Credit {
		t.Fatalf("interim identity or credits: %+v, request %+v", h, request.Header)
	}
	if _, err := wire.DecodeErrorResponse(interim); err != nil {
		t.Fatal(err)
	}
}

func checkLifecycleFinal(t *testing.T, interim, final wire.Message) {
	t.Helper()
	h := final.Header
	if h.Status != smb.StatusSuccess || h.Flags&wire.FlagAsync == 0 || h.AsyncID != interim.Header.AsyncID ||
		h.Command != interim.Header.Command || h.MessageID != interim.Header.MessageID ||
		h.SessionID != interim.Header.SessionID || h.Credit != 0 {
		t.Fatalf("final identity, status or credits: %+v, interim %+v", h, interim.Header)
	}
}

func (c *lifecycleClient) exchange(t *testing.T, request wire.Message) wire.Message {
	t.Helper()
	if err := c.client.Send(c.ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	response := c.receive(t)
	if response.Header.Status == smb.StatusPending {
		checkLifecycleInterim(t, request, response)
		final := c.receive(t)
		checkLifecycleFinal(t, response, final)
		return final
	}
	if response.Header.MessageID != request.Header.MessageID || response.Header.Command != request.Header.Command || response.Header.SessionID != request.Header.SessionID {
		t.Fatalf("reply identity: %+v, request %+v", response.Header, request.Header)
	}
	return response
}

func lifecycleStatus(t *testing.T, response wire.Message, want smb.Status) {
	t.Helper()
	if response.Header.Status != want {
		t.Fatalf("command %d status = %#x, want %#x", response.Header.Command, response.Header.Status, want)
	}
}

func (c *lifecycleClient) create(t *testing.T, name string, disposition, options uint32) wire.CreateResponse {
	t.Helper()
	body, err := wire.EncodeCreateRequest(wire.CreateRequest{Name: name, DesiredAccess: 0x10000000, ShareAccess: 7, Disposition: disposition, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	response := c.exchange(t, c.message(wire.Create, body, 1))
	lifecycleStatus(t, response, smb.StatusSuccess)
	created, err := wire.DecodeCreateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID.Persistent == 0 || created.ID.Volatile == 0 {
		t.Fatal("CREATE returned a zero file ID")
	}
	return created
}

func (c *lifecycleClient) close(t *testing.T, id wire.FileID, wantSize uint64) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	response := c.exchange(t, c.message(wire.Close, body, 1))
	lifecycleStatus(t, response, smb.StatusSuccess)
	closed, err := wire.DecodeCloseResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Flags != 1 || closed.Size != wantSize {
		t.Fatalf("CLOSE attributes = %+v, want size %d", closed, wantSize)
	}
	c.requireClosed(t, id)
}

func (c *lifecycleClient) requireClosed(t *testing.T, id wire.FileID) {
	t.Helper()
	binding := state.Binding{SessionID: c.session.SessionID, TreeID: c.session.TreeID}
	if _, status := c.opens.Find(state.FileID(id), binding); status != smb.StatusFileClosed {
		t.Fatalf("open remains in the table: %#x", status)
	}
}

func (c *lifecycleClient) query(t *testing.T, id wire.FileID) wire.Message {
	t.Helper()
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileStandard), OutputLength: 24})
	if err != nil {
		t.Fatal(err)
	}
	return c.exchange(t, c.message(wire.QueryInfo, body, 1))
}

func (c *lifecycleClient) checkSize(t *testing.T, id wire.FileID, want uint64) {
	t.Helper()
	response := c.query(t, id)
	lifecycleStatus(t, response, smb.StatusSuccess)
	info, err := wire.DecodeQueryInfoResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	// MS-FSCC FileStandardInformation: EndOfFile is at byte 8.
	if len(info.Data) != 24 || binary.LittleEndian.Uint64(info.Data[8:16]) != want || info.Data[20] != 0 || info.Data[21] != 0 {
		t.Fatalf("standard info bytes = %x, want file size %d", info.Data, want)
	}
}

func (c *lifecycleClient) read(t *testing.T, id wire.FileID, want []byte) {
	t.Helper()
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: lifecycleLength(t, want)})
	if err != nil {
		t.Fatal(err)
	}
	response := c.exchange(t, c.message(wire.Read, body, 4))
	checkLifecycleRead(t, response, want)
}

func lifecycleLength(t *testing.T, data []byte) uint32 {
	t.Helper()
	length := len(data)
	if length < 0 || length > math.MaxUint32 {
		t.Fatal("lifecycle payload exceeds the wire length field")
		return 0
	}
	return uint32(length)
}

func checkLifecycleRead(t *testing.T, response wire.Message, want []byte) {
	t.Helper()
	lifecycleStatus(t, response, smb.StatusSuccess)
	read, err := wire.DecodeReadResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read.Data, want) {
		t.Fatalf("READ returned %d bytes, want %d matching bytes", len(read.Data), len(want))
	}
}

func (c *lifecycleClient) write(t *testing.T, id wire.FileID, data []byte) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	response := c.exchange(t, c.message(wire.Write, body, 4))
	checkLifecycleWrite(t, response, lifecycleLength(t, data))
}

func checkLifecycleWrite(t *testing.T, response wire.Message, want uint32) {
	t.Helper()
	lifecycleStatus(t, response, smb.StatusSuccess)
	write, err := wire.DecodeWriteResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if write.Count != want {
		t.Fatalf("WRITE count = %d, want %d", write.Count, want)
	}
}

func (c *lifecycleClient) echo(t *testing.T) {
	t.Helper()
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	request := c.message(wire.Echo, body, 1)
	request.Header.TreeID = 0
	response := c.exchange(t, request)
	lifecycleStatus(t, response, smb.StatusSuccess)
	if _, err := wire.DecodeEchoResponse(response); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolCreateQueryCloseDisconnect(t *testing.T) {
	storage := &lifecycleStorage{Storage: smbtest.NewStorage(t)}
	c := newLifecycleClient(t, storage)
	created := c.create(t, "lifecycle", 2, 0)
	if created.Action != 2 || created.Size != 0 {
		t.Fatalf("new CREATE = %+v", created)
	}
	c.checkSize(t, created.ID, 0)
	data := bytes.Repeat([]byte("acknowledged lifecycle bytes\n"), 5000)
	c.write(t, created.ID, data)
	c.checkSize(t, created.ID, uint64(len(data)))
	c.read(t, created.ID, data)
	c.close(t, created.ID, uint64(len(data)))
	lifecycleStatus(t, c.query(t, created.ID), smb.StatusFileClosed)

	first := c.create(t, "lifecycle", 1, 0)
	second := c.create(t, "left-open", 2, 0)
	c.read(t, first.ID, data)
	body, err := wire.EncodeTreeDisconnectRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	response := c.exchange(t, c.message(wire.TreeDisconnect, body, 1))
	lifecycleStatus(t, response, smb.StatusSuccess)
	if _, err := wire.DecodeTreeDisconnectResponse(response); err != nil {
		t.Fatal(err)
	}
	c.requireClosed(t, first.ID)
	c.requireClosed(t, second.ID)
	if count := storage.closes.Load(); count != 3 {
		t.Fatalf("storage closes = %d, want 3", count)
	}
	lifecycleStatus(t, c.query(t, first.ID), smb.StatusNetworkNameDeleted)
	c.echo(t)
}

func TestProtocolDirectoryOmittedPatternLifecycle(t *testing.T) {
	storage := &lifecycleStorage{Storage: smbtest.NewStorage(t)}
	c := newLifecycleClient(t, storage)
	want := map[string]bool{"keep-a.txt": false, "keep-b.txt": false, "keep-c.txt": false}
	for _, name := range []string{"keep-a.txt", "drop.txt", "keep-b.txt", "keep-c.txt"} {
		created := c.create(t, name, 2, 0)
		c.close(t, created.ID, 0)
	}
	directory := c.create(t, "", 1, 1)
	pattern := "keep-*.txt"
	for page := 0; page <= len(want); page++ {
		flags := uint8(2) // RETURN_SINGLE_ENTRY keeps continuation observable.
		if page == 0 {
			flags |= 1 // RESTART_SCANS
		}
		response := c.directoryPage(t, directory.ID, pattern, flags)
		if page == len(want) {
			lifecycleStatus(t, response, smb.StatusNoMoreFiles)
			break
		}
		lifecycleStatus(t, response, smb.StatusSuccess)
		buffer, err := wire.DecodeQueryDirectoryResponse(response)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := wire.DecodeDirectoryIDFullEntries(buffer.Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("page contains %d entries, want 1", len(entries))
		}
		name := entries[0].Name
		seen, matches := want[name]
		if !matches || seen {
			t.Fatalf("unexpected or repeated entry %q", name)
		}
		want[name] = true
		pattern = ""
	}
	lifecycleStatus(t, c.directoryPage(t, directory.ID, "", 2), smb.StatusNoMoreFiles)
	// Directory sizes are adapter-defined; CLOSE must still return attributes.
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: directory.ID, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	response := c.exchange(t, c.message(wire.Close, body, 1))
	lifecycleStatus(t, response, smb.StatusSuccess)
	closed, err := wire.DecodeCloseResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Flags != 1 || closed.Attributes&0x10 == 0 {
		t.Fatalf("directory CLOSE attributes = %+v", closed)
	}
	c.requireClosed(t, directory.ID)
	lifecycleStatus(t, c.directoryPage(t, directory.ID, "", 2), smb.StatusFileClosed)
	if count := storage.closes.Load(); count != 5 {
		t.Fatalf("storage closes = %d, want 5", count)
	}
	c.echo(t)
}

func (c *lifecycleClient) directoryPage(t *testing.T, id wire.FileID, pattern string, flags uint8) wire.Message {
	t.Helper()
	body, err := wire.EncodeQueryDirectoryRequest(wire.QueryDirectoryRequest{ID: id, Pattern: pattern, Flags: flags, InfoClass: wire.ClassDirectoryIDFull, OutputLength: 256})
	if err != nil {
		t.Fatal(err)
	}
	if pattern == "" && (binary.LittleEndian.Uint16(body[24:26]) != 0 || binary.LittleEndian.Uint16(body[26:28]) != 0) {
		t.Fatal("continuation did not omit the pattern offset and length")
	}
	return c.exchange(t, c.message(wire.QueryDirectory, body, 1))
}

// Only scheduling is controlled. Every data operation and close reaches JuiceFS.
// Configure the gates before sending requests; released gates stay open for reads
// that verify the completed writes.
type lifecycleStorage struct {
	smb.Storage
	gates   map[uint64]chan struct{}
	command wire.Command
	flushes atomic.Uint64
	closes  atomic.Uint64
}

func (s *lifecycleStorage) wait(ctx context.Context, command wire.Command, offset uint64) error {
	if command != s.command {
		return nil
	}
	gate := s.gates[offset]
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *lifecycleStorage) ReadAt(ctx context.Context, handle smb.Handle, dst []byte, offset uint64) (int, error) {
	if err := s.wait(ctx, wire.Read, offset); err != nil {
		return 0, err
	}
	return s.Storage.ReadAt(ctx, handle, dst, offset)
}

func (s *lifecycleStorage) WriteAt(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
	if err := s.wait(ctx, wire.Write, offset); err != nil {
		return 0, err
	}
	return s.Storage.WriteAt(ctx, handle, src, offset)
}

func (s *lifecycleStorage) Flush(ctx context.Context, handle smb.Handle, mode smb.SyncMode) error {
	if err := s.wait(ctx, wire.Flush, s.flushes.Add(1)-1); err != nil {
		return err
	}
	return s.Storage.Flush(ctx, handle, mode)
}

func (s *lifecycleStorage) Close(ctx context.Context, handle smb.Handle) error {
	if err := s.Storage.Close(ctx, handle); err != nil {
		return err
	}
	s.closes.Add(1)
	return nil
}

func TestProtocolPendingOneOpenReverseCompletion(t *testing.T) {
	for _, command := range []wire.Command{wire.Read, wire.Write, wire.Flush} {
		t.Run(fmt.Sprintf("command_%d", command), func(t *testing.T) {
			checkPendingLifecycle(t, command)
		})
	}
}

func checkPendingLifecycle(t *testing.T, command wire.Command) {
	t.Helper()
	adapter := smbtest.NewStorage(t)
	storage := &lifecycleStorage{Storage: adapter, command: command, gates: make(map[uint64]chan struct{})}
	const chunkSize = 70 << 10
	var payload []byte
	for i := 0; i < 3; i++ {
		payload = append(payload, bytes.Repeat([]byte{byte('a' + i)}, chunkSize)...)
		key := uint64(i * chunkSize)
		if command == wire.Flush {
			key = uint64(i)
		}
		storage.gates[key] = make(chan struct{})
	}
	c := newLifecycleClient(t, storage)
	created := c.create(t, "pending", 2, 0)
	if command != wire.Write {
		c.write(t, created.ID, payload)
	}
	requests := make([]wire.Message, 3)
	interims := make([]wire.Message, 3)
	asyncIDs := make(map[uint64]bool)
	for i := range requests {
		start := i * chunkSize
		requests[i] = c.pendingRequest(t, command, created.ID, uint64(start), payload[start:start+chunkSize])
		if err := c.client.Send(c.ctx, []wire.Message{requests[i]}); err != nil {
			t.Fatal(err)
		}
		interims[i] = c.receive(t)
		checkLifecycleInterim(t, requests[i], interims[i])
		id := interims[i].Header.AsyncID
		if asyncIDs[id] {
			t.Fatalf("reused async ID %d on one open", id)
		}
		asyncIDs[id] = true
	}
	c.echo(t)
	for i := len(requests) - 1; i >= 0; i-- {
		key := uint64(i * chunkSize)
		if command == wire.Flush {
			key = uint64(i)
		}
		close(storage.gates[key])
		final := c.receive(t)
		checkLifecycleFinal(t, interims[i], final)
		checkPendingLifecycleBytes(t, command, final, payload[i*chunkSize:(i+1)*chunkSize])
	}
	c.checkSize(t, created.ID, uint64(len(payload)))
	c.read(t, created.ID, payload)
	c.close(t, created.ID, uint64(len(payload)))
	if count := storage.closes.Load(); count != 1 {
		t.Fatalf("storage closes = %d, want 1", count)
	}
	// A duplicate terminal would arrive here instead of this ECHO response.
	c.echo(t)
}

func (c *lifecycleClient) pendingRequest(t *testing.T, command wire.Command, id wire.FileID, offset uint64, data []byte) wire.Message {
	t.Helper()
	var body []byte
	var err error
	charge := uint16(2)
	switch uint16(command) {
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: id, Offset: offset, Length: lifecycleLength(t, data)})
	case uint16(wire.Write):
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Offset: offset, Data: data})
	case uint16(wire.Flush):
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{ID: id})
		charge = 1
	default:
		t.Fatalf("unexpected pending command %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c.message(command, body, charge)
}

func checkPendingLifecycleBytes(t *testing.T, command wire.Command, final wire.Message, want []byte) {
	t.Helper()
	switch uint16(command) {
	case uint16(wire.Read):
		checkLifecycleRead(t, final, want)
	case uint16(wire.Write):
		checkLifecycleWrite(t, final, lifecycleLength(t, want))
	case uint16(wire.Flush):
		if _, err := wire.DecodeFlushResponse(final); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unexpected completed command %d", command)
	}
}
