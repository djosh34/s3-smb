package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

func twoIOClients(t *testing.T, barrier smbfs.MetadataBarrier) (*ioFixture, *readWriteClient, *readWriteClient) {
	t.Helper()
	fixture := newIOFixture(t, barrier)
	a := newReadWriteClient(t, fixture.adapter)
	client, ctx, session := loginClient(t, a.server, smb.CipherAES128GCM, smb.SigningGMAC)
	b := &readWriteClient{client: client, server: a.server, ctx: ctx, session: session, next: session.NextMessageID}
	if a.session.SessionID == b.session.SessionID {
		t.Fatal("independent logins reused a session")
	}
	return fixture, a, b
}

func createIOFile(t *testing.T, client *readWriteClient, request wire.CreateRequest) wire.CreateResponse {
	t.Helper()
	return createdFile(t, client.create(t, request))
}

func closeIOFile(t *testing.T, client *readWriteClient, id wire.FileID) {
	t.Helper()
	response := client.close(t, id, 0)
	requireIOStatus(t, response, smb.StatusSuccess)
	if _, err := wire.DecodeCloseResponse(response); err != nil {
		t.Fatal(err)
	}
	binding := state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID}
	if _, status := client.server.options.State.Find(state.FileID(id), binding); status != smb.StatusFileClosed {
		t.Fatalf("CLOSE retained open: %#x", status)
	}
}

func echoIOClient(t *testing.T, client *readWriteClient) {
	t.Helper()
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.Echo, body, 1)
	requireIOStatus(t, response, smb.StatusSuccess)
	if _, err := wire.DecodeEchoResponse(response); err != nil {
		t.Fatal(err)
	}
}

func TestTwoConnectionDeniedDestructiveCreate(t *testing.T) {
	_, a, b := twoIOClients(t, nil)
	writer := createIOFile(t, a, wire.CreateRequest{Name: "protected", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 1, Disposition: fileCreateDisposition})
	reader := createIOFile(t, b, wire.CreateRequest{Name: "protected", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen})
	payload := "preserve these acknowledged bytes"
	writeCreatedFile(t, a, writer.ID, payload)
	for _, disposition := range []uint32{fileOverwrite, fileSupersede} {
		response := b.create(t, wire.CreateRequest{Name: "protected", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: disposition})
		requireIOStatus(t, response, smb.StatusSharingViolation)
		if _, err := wire.DecodeErrorResponse(response); err != nil {
			t.Fatal(err)
		}
		// Check the existing bytes before proving the rejected CREATE left
		// the connection usable. B reads through its own CREATE-created open.
		readCreatedFile(t, b, reader.ID, payload)
		echoIOClient(t, b)
	}
	closeIOFile(t, a, writer.ID)
	closeIOFile(t, b, reader.ID)
}

func TestTwoConnectionTruncateBeforeStaleFlush(t *testing.T) {
	fixture, a, b := twoIOClients(t, nil)
	writer := createIOFile(t, a, wire.CreateRequest{Name: "flush-data", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileCreateDisposition})
	old := "old buffered bytes that must never reappear after truncation"
	writeCreatedFile(t, a, writer.ID, old)
	if fixture.store.puts.Load() != 0 {
		t.Fatal("test needs buffered writes before the destructive CREATE")
	}
	replacement := createIOFile(t, b, wire.CreateRequest{Name: "flush-data", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileOverwrite})
	if replacement.Size != 0 || replacement.Action != 3 {
		t.Fatalf("OVERWRITE = %+v", replacement)
	}
	requireIOStatus(t, b.read(t, wire.ReadRequest{ID: replacement.ID, Length: 1}, 1), smb.StatusEndOfFile)
	payload := "new"
	writeCreatedFile(t, b, replacement.ID, payload)
	response := ioRoundTrip(a.ctx, t, a.client, flushMessage(t, a.session, a.next, writer.ID, 0))
	a.next++
	requireIOStatus(t, response, smb.StatusSuccess)
	if _, err := wire.DecodeFlushResponse(response); err != nil {
		t.Fatal(err)
	}
	// Check committed length and bytes before any READ can flush again.
	info, eno := fixture.native.Stat(meta.Background(), "/flush-data")
	if eno != 0 {
		t.Fatal(eno)
	}
	if info.Size() != int64(len(payload)) {
		t.Fatalf("stale-handle FLUSH restored length %d, want %d", info.Size(), len(payload))
	}
	assertCommittedFlushData(t, fixture, []byte(payload))
	readCreatedFile(t, b, replacement.ID, payload)
	closeIOFile(t, a, writer.ID)
	closeIOFile(t, b, replacement.ID)
}

func TestTwoConnectionReadAndFlush(t *testing.T) {
	for _, test := range []struct {
		name     string
		reserved uint16
		full     bool
	}{
		{name: "SyncData"},
		{name: "SyncFull", reserved: 0xffff, full: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			commits := make(chan bool, 8)
			barrier := flushBarrier(func(_ context.Context, full bool) error {
				commits <- full
				return nil
			})
			fixture, a, b := twoIOClients(t, barrier)
			writer := createIOFile(t, a, wire.CreateRequest{Name: "flush-data", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileCreateDisposition})
			reader := createIOFile(t, b, wire.CreateRequest{Name: "flush-data", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen})
			payload := "acknowledged bytes from connection A"
			writeCreatedFile(t, a, writer.ID, payload)

			// Knowing A's FileId does not give B access to A's open.
			requireIOStatus(t, b.read(t, wire.ReadRequest{ID: writer.ID, Length: 1}, 1), smb.StatusFileClosed)
			echoIOClient(t, b)
			readCreatedFile(t, b, reader.ID, payload)
			// READ can upload the shared writer. Complete another write so the
			// FLUSH has work that B has not already committed by reading.
			putsBefore := fixture.store.puts.Load()
			payload = "A completed another write before B's FLUSH"
			writeCreatedFile(t, a, writer.ID, payload)

			message := flushMessage(t, b.session, b.next, reader.ID, test.reserved)
			b.next++
			response := ioRoundTrip(b.ctx, t, b.client, message)
			requireIOStatus(t, response, smb.StatusSuccess)
			if _, err := wire.DecodeFlushResponse(response); err != nil {
				t.Fatal(err)
			}
			select {
			case full := <-commits:
				if full != test.full {
					t.Fatalf("metadata barrier full = %v, want %v", full, test.full)
				}
			default:
				t.Fatal("FLUSH replied without a metadata barrier")
			}
			if fixture.store.puts.Load() <= putsBefore {
				t.Fatal("B's FLUSH did not upload A's completed writes")
			}
			assertCommittedFlushData(t, fixture, []byte(payload))
			closeIOFile(t, a, writer.ID)
			// Closing A must not release B's separate storage reference.
			readCreatedFile(t, b, reader.ID, payload)
			closeIOFile(t, b, reader.ID)
		})
	}
}
