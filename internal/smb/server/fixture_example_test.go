package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFixtureReadSeesWriteFromAnotherOpen(t *testing.T) {
	client := newTestServer(t).connect(t)
	writer := client.open(t, "coherent")
	reader := client.open(t, "coherent")
	if status := client.write(t, wire.WriteRequest{ID: writer, Offset: 2, Data: []byte("hello")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	for _, test := range []struct {
		name, data      string
		offset          uint64
		length, minimum uint32
		want            smb.Status
	}{
		{"short read", "hello", 2, 10, 0, smb.StatusSuccess},
		{"minimum met", "hello", 2, 10, 5, smb.StatusSuccess},
		{"minimum unmet", "", 2, 10, 6, smb.StatusEndOfFile},
		{"minimum exceeds request", "", 2, 3, 4, smb.StatusEndOfFile},
		{"at EOF", "", 7, 1, 0, smb.StatusEndOfFile},
		{"past EOF", "", 8, 1, 0, smb.StatusEndOfFile},
		{"zero length", "", 7, 0, 0, smb.StatusSuccess},
		{"zero length minimum", "", 7, 0, 1, smb.StatusEndOfFile},
		{"hole", "\x00\x00", 0, 2, 0, smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, status := client.read(t, wire.ReadRequest{ID: reader, Offset: test.offset, Length: test.length, MinimumCount: test.minimum})
			if status != test.want || string(data) != test.data {
				t.Fatalf("READ = %q, %#x; want %q, %#x", data, status, test.data, test.want)
			}
		})
	}
}

// S3 holds the upload's response, so the request stays in storage until the
// test releases it. Meanwhile the connection must keep serving other requests.
func TestFixtureSlowS3UploadRepliesAsync(t *testing.T) {
	for _, test := range []struct {
		name    string
		command wire.Command
	}{{"write", wire.Write}, {"flush", wire.Flush}} {
		t.Run(test.name, func(t *testing.T) {
			adapter, proxy := smbtest.NewS3Storage(t, smbtest.S3Config{Timeout: time.Minute})
			client := newTestServerOn(t, adapter).connect(t)
			t.Cleanup(proxy.Release)
			other := client.open(t, "other")
			otherData := []byte("unrelated bytes")
			if status := client.write(t, wire.WriteRequest{ID: other, Data: otherData, Flags: writeThrough}); status != smb.StatusSuccess {
				t.Fatalf("WRITE status %#x", status)
			}
			slow := client.open(t, "slow")
			held, err := proxy.HoldNextChunkResponse()
			if err != nil {
				t.Fatal(err)
			}
			// More than one credit also checks credits for multi-credit async I/O.
			data := bytes.Repeat([]byte("slow storage bytes\n"), 4000)
			var request wire.Header
			if test.command == wire.Write {
				body := encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: slow, Data: data, Flags: writeThrough})
				request = client.send(t, wire.Write, body, creditsFor(len(data)))
			} else {
				data = data[:32]
				if status := client.write(t, wire.WriteRequest{ID: slow, Data: data}); status != smb.StatusSuccess {
					t.Fatalf("WRITE status %#x", status)
				}
				request = client.send(t, wire.Flush, encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: slow}), 1)
			}
			<-held
			// The interim reply grants the request's credits; the final one grants none.
			if interim := client.interim(t, request); interim.Credit != request.Credit {
				t.Fatalf("interim reply granted %d credits, want %d", interim.Credit, request.Credit)
			}
			client.echo(t)
			if got, status := client.read(t, wire.ReadRequest{ID: other, Length: 1024}); status != smb.StatusSuccess || !bytes.Equal(got, otherData) {
				t.Fatalf("unrelated READ = %q, %#x", got, status)
			}
			proxy.Release()
			if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
				t.Fatalf("final status %#x", status)
			}
			if got, status := client.read(t, wire.ReadRequest{ID: slow, Length: 1 << 17}); status != smb.StatusSuccess || !bytes.Equal(got, data) {
				t.Fatalf("READ after upload = %d bytes, %#x", len(got), status)
			}
		})
	}
}

func TestFixtureDurableReconnectDuringIO(t *testing.T) {
	for _, protection := range []struct {
		name   string
		cipher uint16
	}{{"signed", 0}, {"encrypted", smb.CipherAES256GCM}} {
		for _, operation := range []struct {
			name    string
			command wire.Command
		}{{"write", wire.Write}, {"read", wire.Read}, {"flush", wire.Flush}} {
			t.Run(protection.name+"/"+operation.name, func(t *testing.T) {
				checkFixtureDurableReconnectIO(t, protection.cipher, operation.command)
			})
		}
	}
}

func checkFixtureDurableReconnectIO(t *testing.T, cipher uint16, command wire.Command) {
	t.Helper()
	srv := newTestServer(t)
	client := srv.dial(t, smbtest.LoginOptions{Cipher: cipher, Signing: smb.SigningGMAC})
	request := wire.CreateRequest{Name: "band", DesiredAccess: fileAllAccess, ShareAccess: 1, Disposition: fileOpenIf}
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{74}, State: smb.LeaseRead | smb.LeaseHandle}
	durable := wire.DurableRequest{CreateGUID: [16]byte{75}, Timeout: 120000}
	created, status := client.create(t, smbtest.CreateOptions{Request: request, Lease: &lease, Durable: &durable})
	if status != smb.StatusSuccess || created.Lease == nil || created.Lease.State != lease.State || created.Durable == nil || created.Durable.Timeout != durable.Timeout {
		t.Fatalf("durable CREATE = %+v, %#x", created, status)
	}
	id := created.Reply.ID
	acknowledged := []byte("acknowledged data")
	exclusive := wire.LockRequest{ID: id, Elements: []wire.LockElement{{Length: 17, Flags: 2}}}
	if status = client.write(t, wire.WriteRequest{ID: id, Data: acknowledged}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	if status = client.lock(t, exclusive); status != smb.StatusSuccess {
		t.Fatalf("LOCK status %#x", status)
	}
	peer := srv.connect(t)
	peerOpen, status := peer.create(t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "band", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen}})
	if status != smb.StatusSuccess {
		t.Fatalf("peer CREATE status %#x", status)
	}
	peerLock := wire.LockRequest{ID: peerOpen.Reply.ID, Elements: exclusive.Elements}

	// The request blocks in storage until the cut cancels it.
	entered, canceled := make(chan struct{}), make(chan struct{})
	block := func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	srv.faults.set(func(hooks *storageHooks) {
		hooks.WriteAt = func(ctx context.Context, _ smb.Handle, _ []byte, _ uint64) (int, error) { return 0, block(ctx) }
		hooks.ReadAt = func(ctx context.Context, _ smb.Handle, _ []byte, _ uint64) (int, error) { return 0, block(ctx) }
		hooks.Flush = func(ctx context.Context, _ smb.Handle, _ smb.SyncMode) error { return block(ctx) }
	})
	switch uint16(command) {
	case uint16(wire.Write):
		client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: acknowledged}), 1)
	case uint16(wire.Read):
		client.send(t, wire.Read, encode(t, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Length: 17}), 1)
	case uint16(wire.Flush):
		client.send(t, wire.Flush, encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: id}), 1)
	}
	<-entered
	client.drop(t)
	<-canceled
	srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
	for {
		reply, err := client.raw.Receive(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.Status != smb.StatusPending {
			t.Fatalf("old request completed after the cut: %+v, %v", reply.Messages, err)
		}
	}

	// A peer CREATE could break H and close the detached open, so check its
	// sharing in the open table without starting a break.
	selected, err := srv.adapter.Lookup(t.Context(), "band")
	if err != nil {
		t.Fatal(err)
	}
	table := srv.server.options.State
	token, status := table.Reserve(state.OpenRequest{
		Object: selected.Object, Binding: state.Binding{SessionID: peer.session.SessionID, TreeID: peer.session.TreeID},
		GrantedAccess: fileWriteData, Sharing: 7,
	})
	if status == smb.StatusSuccess {
		if abortStatus := table.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
	}
	if status != smb.StatusSharingViolation {
		t.Fatalf("detached sharing = %#x", status)
	}
	if status = peer.lock(t, peerLock); status != smb.StatusLockNotGranted {
		t.Fatalf("detached range = %#x", status)
	}

	srv.clock.advance(30 * time.Second)
	resumed := client.reconnect(t)
	reopened, status := resumed.create(t, smbtest.CreateOptions{Request: request, Lease: created.Lease, Reconnect: &wire.DurableReconnect{ID: id, CreateGUID: durable.CreateGUID}})
	if status != smb.StatusSuccess || reopened.Reply.ID.Persistent != id.Persistent || reopened.Reply.ID.Volatile == id.Volatile {
		t.Fatalf("DH2C = %+v, %#x", reopened.Reply, status)
	}
	newID := reopened.Reply.ID
	if _, status := resumed.read(t, wire.ReadRequest{ID: id, Length: 17}); status != smb.StatusFileClosed {
		t.Fatalf("READ on the old volatile ID = %#x", status)
	}
	if data, status := resumed.read(t, wire.ReadRequest{ID: newID, Length: 17}); status != smb.StatusSuccess || !bytes.Equal(data, acknowledged) {
		t.Fatalf("reconnected READ = %q, %#x", data, status)
	}
	if status := peer.lock(t, peerLock); status != smb.StatusLockNotGranted {
		t.Fatalf("reattached range = %#x", status)
	}
	if status := resumed.write(t, wire.WriteRequest{ID: newID, Offset: 17, Data: []byte(" continued")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	if status := resumed.flush(t, wire.FlushRequest{ID: newID}); status != smb.StatusSuccess {
		t.Fatalf("FLUSH status %#x", status)
	}
	want := "acknowledged data continued"
	if data, status := resumed.read(t, wire.ReadRequest{ID: newID, Length: 1024}); status != smb.StatusSuccess || string(data) != want {
		t.Fatalf("READ after reconnect = %q, %#x", data, status)
	}
	unlock := wire.LockRequest{ID: newID, Elements: []wire.LockElement{{Length: 17, Flags: 4}}}
	if status := resumed.lock(t, unlock); status != smb.StatusSuccess {
		t.Fatalf("retained owner's unlock = %#x", status)
	}
	if status := peer.lock(t, peerLock); status != smb.StatusSuccess {
		t.Fatalf("peer lock after unlock = %#x", status)
	}
}

func TestFixtureCreateWaitsForLeaseBreakBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition uint32
		target      uint32
		size        uint64
	}{
		{name: "write", disposition: fileOpen, target: smb.LeaseRead | smb.LeaseHandle, size: 17},
		{name: "overwrite", disposition: fileOverwrite, target: 0, size: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkFixtureCreateWaitsForLeaseBreak(t, test.disposition, test.target, test.size)
		})
	}
}

func checkFixtureCreateWaitsForLeaseBreak(t *testing.T, disposition, target uint32, size uint64) {
	t.Helper()
	srv := newTestServer(t)
	holder, opener := srv.connect(t), srv.connect(t)
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{1}, State: smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle}
	held, status := holder.create(t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "file", DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileOpenIf}, Lease: &lease})
	if status != smb.StatusSuccess || held.Lease == nil || held.Lease.State != lease.State {
		t.Fatalf("lease grant = %+v, %#x", held.Lease, status)
	}
	if status = holder.write(t, wire.WriteRequest{ID: held.Reply.ID, Data: []byte("seventeen bytes!!")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	request := opener.sendCreate(t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "file", DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: disposition}})
	notification := holder.leaseBreak(t)
	if notification.Key != lease.Key || notification.CurrentState != lease.State || notification.NewState != target {
		t.Fatalf("break = %+v", notification)
	}
	// The CREATE goes async and changes nothing until the holder acknowledges.
	opener.interim(t, request)
	resolved, err := srv.adapter.Lookup(t.Context(), "file")
	if err != nil {
		t.Fatal(err)
	}
	if attr, attrErr := srv.adapter.GetAttr(t.Context(), resolved.Object); attrErr != nil || attr.Size != 17 {
		t.Fatalf("size before ACK = %d, %v", attr.Size, attrErr)
	}
	if status = holder.ackLease(t, notification.Key, notification.NewState); status != smb.StatusSuccess {
		t.Fatalf("lease ACK status %#x", status)
	}
	opened, status := opener.created(t, request)
	if status != smb.StatusSuccess || opened.Reply.Size != size {
		t.Fatalf("CREATE after ACK = %+v, %#x", opened.Reply, status)
	}
	if status = holder.close(t, held.Reply.ID); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
	if status = opener.close(t, opened.Reply.ID); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
}

func TestFixtureExpiryDeletesDetachedDeleteOnCloseFile(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{2}, State: smb.LeaseRead | smb.LeaseHandle}
	durable := wire.DurableRequest{CreateGUID: [16]byte{3}, Timeout: 120000}
	request := wire.CreateRequest{Name: "band", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf, Options: fileDeleteOnClose}
	created, status := client.create(t, smbtest.CreateOptions{Request: request, Lease: &lease, Durable: &durable})
	if status != smb.StatusSuccess || created.Durable == nil {
		t.Fatalf("durable CREATE = %+v, %#x", created, status)
	}
	client.drop(t)
	exists := func() bool {
		resolved, err := srv.adapter.Lookup(t.Context(), "band")
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Exists
	}
	srv.clock.advance(2*time.Minute - time.Nanosecond)
	srv.expire(t)
	if !exists() {
		t.Fatal("detached file deleted before its durable timeout")
	}
	srv.clock.advance(time.Nanosecond)
	srv.expire(t)
	if exists() {
		t.Fatal("expiry kept a delete-on-close file")
	}
}

func TestFixtureSetInfoAllocationBelowEOFShrinks(t *testing.T) {
	for _, allocation := range []uint64{0, 3, 7, 8192} {
		t.Run(fmt.Sprint(allocation), func(t *testing.T) {
			client := newTestServer(t).connect(t)
			id := client.open(t, "file")
			if status := client.write(t, wire.WriteRequest{ID: id, Data: []byte("1234567")}); status != smb.StatusSuccess {
				t.Fatalf("WRITE status %#x", status)
			}
			input := encode(t, wire.EncodeFileAllocationInformation, wire.FileAllocationInformation{AllocationSize: allocation})
			if status := client.setInfo(t, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileAllocation), Input: input}); status != smb.StatusSuccess {
				t.Fatalf("SET_INFO status %#x", status)
			}
			// Allocation rounds up to whole clusters, so only zero is below EOF.
			want := uint64(7)
			if allocation == 0 {
				want = 0
			}
			endOfFile := func() uint64 {
				data, status := client.queryInfo(t, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileStandard), OutputLength: 1024})
				if status != smb.StatusSuccess {
					t.Fatalf("QUERY_INFO status %#x", status)
				}
				standard, err := wire.DecodeFileStandardInformation(data)
				if err != nil {
					t.Fatal(err)
				}
				return standard.EndOfFile
			}
			if got := endOfFile(); got != want {
				t.Fatalf("EOF = %d, want %d", got, want)
			}
			if status := client.flush(t, wire.FlushRequest{ID: id}); status != smb.StatusSuccess {
				t.Fatalf("FLUSH status %#x", status)
			}
			if got := endOfFile(); got != want {
				t.Fatalf("EOF after flush = %d, want %d", got, want)
			}
		})
	}
}

func TestFixtureDirectoryEmptyRootListsDots(t *testing.T) {
	client := newTestServer(t).connect(t)
	root, status := client.create(t, smbtest.CreateOptions{Request: wire.CreateRequest{DesiredAccess: fileGenericRead, ShareAccess: 7, Disposition: fileOpen, Options: fileDirectoryFile}})
	if status != smb.StatusSuccess {
		t.Fatalf("root CREATE status %#x", status)
	}
	query := wire.QueryDirectoryRequest{ID: root.Reply.ID, Pattern: "*", Flags: directoryReopen, InfoClass: wire.ClassDirectoryNames, OutputLength: 4096}
	data, status := client.queryDirectory(t, query)
	if status != smb.StatusSuccess {
		t.Fatalf("QUERY_DIRECTORY status %#x", status)
	}
	entries, err := wire.DecodeDirectoryNamesEntries(data)
	if err != nil || len(entries) != 2 || entries[0].Name != "." || entries[1].Name != ".." {
		t.Fatalf("empty root lists %+v, %v", entries, err)
	}
	query.Pattern, query.Flags = "", 0
	if _, status = client.queryDirectory(t, query); status != smb.StatusNoMoreFiles {
		t.Fatalf("continuation status %#x", status)
	}
	if status = client.close(t, root.Reply.ID); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
}

func TestFixtureIOCTLOnOpenFileIsRefused(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	// FSCTL_SRV_REQUEST_RESUME_KEY, which server-side copy would need.
	if status := client.ioctl(t, wire.IOCTLRequest{ID: id, ControlCode: 0x00140078, Flags: 1, MaxOutput: 1024}); status != smb.StatusNotSupported {
		t.Fatalf("IOCTL status %#x", status)
	}
	client.echo(t)
}
