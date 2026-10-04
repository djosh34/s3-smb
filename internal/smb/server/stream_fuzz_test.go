package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// FuzzServerStream sends the server arbitrary bytes as a client would, then
// ends the connection. The server must answer in valid frames, end the
// connection and shut down without panicking or hanging.
func FuzzServerStream(f *testing.F) {
	for _, seed := range streamSeeds(f) {
		f.Add(seed.stream)
	}
	adapter := smbtest.NewStorage(f)
	root, err := adapter.Lookup(f.Context(), "")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) > 64<<10 {
			t.Skip("stream over 64 KiB")
		}
		// Cleanups run last first: the server shuts down before the reset.
		t.Cleanup(func() {
			if err := resetStorage(context.WithoutCancel(t.Context()), adapter, root.Attr); err != nil {
				t.Error(err)
			}
		})
		runStream(t, newTestServerOn(t, adapter), stream)
	})
}

// The seeds must reach the server's handlers, or fuzzing tests nothing past
// the frame decoder.
func TestServerStreamSeeds(t *testing.T) {
	adapter := smbtest.NewStorage(t)
	for _, seed := range streamSeeds(t) {
		var statuses []smb.Status
		for _, reply := range runStream(t, newTestServerOn(t, adapter), seed.stream) {
			statuses = append(statuses, reply.Header.Status)
		}
		if !slices.Equal(statuses, seed.want) {
			t.Errorf("%s: statuses %#x, want %#x", seed.name, statuses, seed.want)
		}
	}
}

type streamSeed struct {
	name   string
	stream []byte
	want   []smb.Status
}

func streamSeeds(t testing.TB) []streamSeed {
	request := func(command wire.Command, id, session uint64, body []byte) wire.Message {
		return wire.Message{Header: wire.Header{Command: command, MessageID: id, SessionID: session, CreditCharge: 1, Credit: 16}, Body: body}
	}
	echo := encode(t, wire.EncodeEchoRequest, wire.EmptyRequest{})
	negotiate := frame(t, request(wire.Negotiate, 0, 0, negotiateRequest(t)))
	setup := frame(t, request(wire.SessionSetup, 1, 0, loginStart(t)))
	// Session 1 is still authenticating, so it has no tree yet.
	tree := frame(t, request(wire.TreeConnect, 2, 1, encode(t, wire.EncodeTreeConnectRequest, wire.TreeConnectRequest{Path: `\\host\backup`})))
	join := func(frames ...[]byte) []byte { return bytes.Join(frames, nil) }
	ok := smb.StatusSuccess
	return []streamSeed{
		{"negotiate", negotiate, []smb.Status{ok}},
		{"echo", join(negotiate, frame(t, request(wire.Echo, 1, 0, echo)), frame(t, request(wire.Echo, 2, 0, echo))), []smb.Status{ok, ok, ok}},
		{"compound", join(negotiate, frame(t, request(wire.Echo, 1, 0, echo), request(wire.Echo, 2, 0, echo))), []smb.Status{ok, ok, ok}},
		{"session setup", join(negotiate, setup), []smb.Status{ok, smb.StatusMoreProcessingRequired}},
		{"tree connect", join(negotiate, setup, tree), []smb.Status{ok, smb.StatusMoreProcessingRequired, smb.StatusUserSessionDeleted}},
	}
}

// streamConn is a client connection that sends stream and then closes its
// side. It keeps what the server writes.
type streamConn struct {
	net.Conn
	stream  io.Reader
	written bytes.Buffer
	mu      sync.Mutex
}

func (conn *streamConn) Read(data []byte) (int, error) { return conn.stream.Read(data) }

func (conn *streamConn) Write(data []byte) (int, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.written.Write(data)
}

func (*streamConn) Close() error { return nil }

// runStream serves stream on one connection and returns the replies.
func runStream(t *testing.T, srv *testServer, stream []byte) []wire.Message {
	t.Helper()
	conn := &streamConn{stream: bytes.NewReader(stream)}
	if err := srv.server.ServeConn(t.Context(), conn); err != nil {
		t.Logf("server ended the connection: %v", err)
	}
	conn.mu.Lock()
	written := bytes.NewReader(conn.written.Bytes())
	conn.mu.Unlock()
	var replies []wire.Message
	for written.Len() > 0 {
		payload, err := readFrame(written, smb.MaxReadSize+smb.CreditUnit)
		if err != nil {
			t.Fatal(err)
		}
		messages, err := wire.Split(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if message.Header.Flags&wire.FlagResponse == 0 {
				t.Fatalf("reply without the response flag: %+v", message.Header)
			}
		}
		replies = append(replies, messages...)
	}
	return replies
}

// resetStorage removes everything below the root and restores its
// attributes, so each fuzz input starts from the same storage.
func resetStorage(ctx context.Context, adapter *smbfs.FS, root smb.Attr) error {
	if err := removeChildren(ctx, adapter, root.Inode); err != nil {
		return err
	}
	return adapter.SetAttr(ctx, smb.ObjectKey{Inode: root.Inode}, smb.AttrChange{
		Created: &root.Created, Accessed: &root.Accessed, Modified: &root.Modified, Changed: &root.Changed, Attributes: &root.Attributes,
	})
}

func removeChildren(ctx context.Context, adapter *smbfs.FS, parent smb.Inode) error {
	for {
		entries, err := adapter.ReadDir(ctx, parent, 0, 64)
		if err != nil || len(entries) == 0 {
			return err
		}
		for _, entry := range entries {
			if entry.Attr.Kind == smb.KindDirectory {
				if err := removeChildren(ctx, adapter, entry.Attr.Inode); err != nil {
					return err
				}
			}
			if err := adapter.Remove(ctx, smb.Name{Parent: parent, Base: entry.Name}, entry.Attr.Inode); err != nil {
				return fmt.Errorf("remove %q: %w", entry.Name, err)
			}
		}
	}
}
