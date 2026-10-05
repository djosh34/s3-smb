package server

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// CANCEL names a waiting request by message or async ID. It counts only when
// it passes the session's signing or encryption and comes from the session of
// the request. It never gets a reply.
func TestCancel(t *testing.T) {
	protected := func(t *testing.T, client *testClient, cancel wire.Message) {
		if err := client.raw.Send(t.Context(), []wire.Message{cancel}); err != nil {
			t.Fatal(err)
		}
	}
	unprotected := func(t *testing.T, client *testClient, cancel wire.Message) { client.sendUnprotected(t, cancel) }
	forged := func(t *testing.T, client *testClient, cancel wire.Message) {
		cancel.Header.Flags |= wire.FlagSigned
		cancel.Header.Signature = [16]byte{1}
		client.sendUnprotected(t, cancel)
	}
	otherSession := func(t *testing.T, client *testClient, cancel wire.Message) {
		cancel.Header.SessionID++
		client.sendUnprotected(t, cancel)
	}
	for _, test := range []struct {
		send        func(t *testing.T, client *testClient, cancel wire.Message)
		name        string
		cipher      uint16
		byMessageID bool
		cancels     bool
	}{
		{protected, "by message ID", 0, true, true},
		{protected, "by async ID, encrypted", smb.CipherAES128GCM, false, true},
		{unprotected, "unsigned", 0, false, true},
		{forged, "forged signature", 0, false, false},
		{unprotected, "plaintext in an encrypted session", smb.CipherAES128GCM, false, false},
		{otherSession, "another session", 0, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := dialProtected(t, test.cipher)
			id := client.open(t, "file")
			entered, release := client.server.holdWrites()
			request := client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: []byte("data")}), 1)
			<-entered
			interim := client.interim(t, request)
			header := wire.Header{Command: wire.Cancel, SessionID: client.session.SessionID, MessageID: request.MessageID}
			if !test.byMessageID {
				header.Flags, header.AsyncID = wire.FlagAsync, interim.AsyncID
			}
			test.send(t, client, wire.Message{Header: header, Body: encode(t, wire.EncodeCancelRequest, wire.EmptyRequest{})})
			client.echo(t) // The server takes requests in order, so it has handled CANCEL.
			want := smb.StatusCancelled
			if !test.cancels {
				close(release)
				want = smb.StatusSuccess
			}
			if status := client.receive(t, request).Header.Status; status != want {
				t.Fatalf("WRITE status %#x, want %#x", status, want)
			}
			client.noExtraReplies(t)
		})
	}
}

// A storage call that fails because its request was cancelled, as JuiceFS does
// with EINTR, still answers STATUS_CANCELLED. WRITE returns the error;
// SET_INFO turns it into a status itself.
func TestCancelledStorageErrorAnswersCancelled(t *testing.T) {
	for _, test := range []struct {
		hook func(hooks *storageHooks, fail func(context.Context) error)
		send func(t *testing.T, client *testClient, id wire.FileID) wire.Header
		name string
	}{
		{func(hooks *storageHooks, fail func(context.Context) error) {
			hooks.WriteAt = func(ctx context.Context, _ smb.Handle, _ []byte, _ uint64) (int, error) { return 0, fail(ctx) }
		}, func(t *testing.T, client *testClient, id wire.FileID) wire.Header {
			return client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: []byte("data")}), 1)
		}, "WRITE"},
		{func(hooks *storageHooks, fail func(context.Context) error) {
			hooks.PathOf = func(ctx context.Context, _ smb.Inode) (string, error) { return "", fail(ctx) }
		}, func(t *testing.T, client *testClient, id wire.FileID) wire.Header {
			rename := encode(t, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: "moved"})
			body := encode(t, wire.EncodeSetInfoRequest, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileRename), Input: rename})
			return client.send(t, wire.SetInfo, body, 1)
		}, "SET_INFO rename"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			id := client.open(t, "file")
			entered := make(chan struct{})
			srv.faults.set(func(hooks *storageHooks) {
				test.hook(hooks, func(ctx context.Context) error {
					close(entered)
					<-ctx.Done()
					return smb.ErrIO
				})
			})
			request := test.send(t, client, id)
			<-entered
			client.cancelAsync(t, client.interim(t, request))
			if status := client.receive(t, request).Header.Status; status != smb.StatusCancelled {
				t.Fatalf("status %#x", status)
			}
			client.noExtraReplies(t)
		})
	}
}

// Cancelling a related member that waits for the one before it leaves that
// one running.
func TestCancelWaitingRelatedRequest(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := client.open(t, "file")
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Flush = func(context.Context, smb.Handle, smb.SyncMode) error {
			t.Error("the cancelled FLUSH reached storage")
			return nil
		}
	})
	entered, release := srv.holdWrites()
	write := message(t, client, wire.Write, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: []byte("data")})
	flush := related(message(t, client, wire.Flush, wire.EncodeFlushRequest, wire.FlushRequest{ID: placeholder}))
	if err := client.raw.Send(t.Context(), []wire.Message{write, flush}); err != nil {
		t.Fatal(err)
	}
	<-entered
	client.interim(t, write.Header)
	client.cancelAsync(t, client.interim(t, flush.Header))
	if status := client.receive(t, flush.Header).Header.Status; status != smb.StatusCancelled {
		t.Fatalf("FLUSH status %#x", status)
	}
	close(release)
	if status := client.receive(t, write.Header).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	srv.expectContent(t, map[string]string{"file": "data"})
	client.noExtraReplies(t)
}

// However a session's work is cut short, the server cancels its requests and
// waits for them before closing their files, even for a storage call that
// ignores the cancellation and a related request still waiting for it. A
// reply that still goes out after LOGOFF uses the ended session's keys.
func TestCleanupWaitsForRunningRequests(t *testing.T) {
	drop := func(t *testing.T, _ *testServer, client *testClient) { client.drop(t) }
	shutdown := func(t *testing.T, srv *testServer, client *testClient) {
		if err := srv.server.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := client.ended(); !errors.Is(err, context.Canceled) {
			t.Fatalf("connection error %v", err)
		}
	}
	for _, test := range []struct {
		cut     func(t *testing.T, srv *testServer, client *testClient)
		name    string
		cipher  uint16
		command wire.Command // LOGOFF or TREE_DISCONNECT, if cut is nil
	}{
		{nil, "LOGOFF", 0, wire.Logoff},
		{nil, "LOGOFF, encrypted", smb.CipherAES128GCM, wire.Logoff},
		{nil, "TREE_DISCONNECT", smb.CipherAES128GCM, wire.TreeDisconnect},
		{drop, "network cut", 0, 0},
		{shutdown, "shutdown", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := dialProtected(t, test.cipher)
			srv := client.server
			id := client.open(t, "file")
			var written atomic.Bool
			var closes atomic.Int32
			srv.faults.set(func(hooks *storageHooks) {
				hooks.WriteAt = func(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
					<-ctx.Done()
					n, err := srv.adapter.WriteAt(context.WithoutCancel(ctx), handle, src, offset)
					written.Store(true)
					return n, err
				}
				hooks.Flush = func(context.Context, smb.Handle, smb.SyncMode) error {
					t.Error("the cancelled FLUSH reached storage")
					return nil
				}
				hooks.Close = func(ctx context.Context, handle smb.Handle) error {
					if !written.Load() {
						t.Error("the file closed while WRITE was using it")
					}
					closes.Add(1)
					return srv.adapter.Close(ctx, handle)
				}
			})
			write := message(t, client, wire.Write, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: []byte("data")})
			flush := related(message(t, client, wire.Flush, wire.EncodeFlushRequest, wire.FlushRequest{ID: placeholder}))
			if err := client.raw.Send(t.Context(), []wire.Message{write, flush}); err != nil {
				t.Fatal(err)
			}
			client.interim(t, write.Header)
			client.interim(t, flush.Header)
			if test.cut != nil {
				test.cut(t, srv, client)
				// The connection is gone; nothing came after the interim replies.
				if reply, err := client.raw.Receive(t.Context()); !errors.Is(err, io.EOF) {
					t.Fatalf("reply %+v, %v", reply.Messages, err)
				}
			} else {
				// LOGOFF and TREE_DISCONNECT have the same empty body.
				end := message(t, client, test.command, wire.EncodeLogoffRequest, wire.EmptyRequest{})
				statuses := sendCompound(t, client, end)
				statuses = append(finalStatuses(t, client, []wire.Message{write, flush}), statuses...)
				if want := []smb.Status{smb.StatusSuccess, smb.StatusCancelled, smb.StatusSuccess}; !slices.Equal(statuses, want) {
					t.Fatalf("WRITE, FLUSH, %v statuses %#x", test.command, statuses)
				}
			}
			if closes.Load() != 1 {
				t.Fatalf("%d closes", closes.Load())
			}
			srv.expectContent(t, map[string]string{"file": "data"})
		})
	}
}
