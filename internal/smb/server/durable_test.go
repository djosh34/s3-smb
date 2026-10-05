package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// durableCreate opens or creates name with full access, sharing everything,
// an RH lease and a durable handle, both keyed by key.
func durableCreate(name string, key byte) smbtest.CreateOptions {
	options := leasedCreate(name, key, leaseRH)
	options.Request.DesiredAccess = fileAllAccess
	options.Durable = &wire.DurableRequest{CreateGUID: [16]byte{key}}
	return options
}

// reclaimCreate is the DH2C that reclaims the open created by options.
func reclaimCreate(options smbtest.CreateOptions, created smbtest.CreateResult) smbtest.CreateOptions {
	lease := *created.Lease
	options.Lease = &lease
	options.Reconnect = &wire.DurableReconnect{ID: created.Reply.ID, CreateGUID: options.Durable.CreateGUID}
	options.Durable = nil
	return options
}

func createContext[T any](t *testing.T, encoder func(T) (wire.CreateContext, error), value T) wire.CreateContext {
	t.Helper()
	context, err := encoder(value)
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func (s *testServer) exists(t *testing.T, name string) bool {
	t.Helper()
	resolved, err := s.adapter.Lookup(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Exists
}

func TestDurableTimeouts(t *testing.T) {
	client := newTestServer(t).connect(t)
	for key, test := range []struct{ requested, granted uint32 }{
		{0, 120000}, {1, 1}, {960000, 960000}, {960001, 960000},
	} {
		options := durableCreate("file", byte(key+1))
		options.Request.Name = string(rune('a' + key))
		options.Durable.Timeout = test.requested
		options.Durable.Flags = 2 // Persistent, which is never granted.
		result := mustCreate(t, client, options)
		if result.Durable == nil || *result.Durable != (wire.DurableReply{Timeout: test.granted}) {
			t.Fatalf("requested %d ms: durable grant %+v, want %d ms", test.requested, result.Durable, test.granted)
		}
	}
}

func TestDurableNeedsHandleLeaseOnRegularFile(t *testing.T) {
	client := newTestServer(t).connect(t)
	client.open(t, "base")
	key := byte(0)
	for _, test := range []struct {
		modify func(*smbtest.CreateOptions)
		name   string
	}{
		{name: "no lease", modify: func(o *smbtest.CreateOptions) { o.Lease = nil }},
		{name: "read lease", modify: func(o *smbtest.CreateOptions) { o.Lease.State = leaseR }},
		{name: "directory", modify: func(o *smbtest.CreateOptions) { o.Request.Options = fileDirectoryFile }},
		{name: "stream", modify: func(o *smbtest.CreateOptions) { o.Request.Name = "base:meta" }},
		{name: "durable v1", modify: func(o *smbtest.CreateOptions) {
			o.Durable = nil
			o.Request.Contexts = []wire.CreateContext{{Name: "DHnQ", Data: make([]byte, 16)}}
		}},
	} {
		key++
		t.Run(test.name, func(t *testing.T) {
			options := durableCreate(test.name, key)
			test.modify(&options)
			if result := mustCreate(t, client, options); result.Durable != nil {
				t.Fatalf("durable grant %+v", result.Durable)
			}
		})
	}
}

func TestCreateContextsFailBeforeNamespaceChange(t *testing.T) {
	durable := createContext(t, wire.EncodeDurableRequest, wire.DurableRequest{CreateGUID: [16]byte{5}})
	reconnect := createContext(t, wire.EncodeDurableReconnect, wire.DurableReconnect{CreateGUID: [16]byte{5}})
	srv := newTestServer(t)
	client := srv.connect(t)
	for _, test := range []struct {
		name     string
		contexts []wire.CreateContext
	}{
		{"short lease", []wire.CreateContext{{Name: "RqLs", Data: []byte{1}}}},
		{"two leases", []wire.CreateContext{{Name: "RqLs", Data: make([]byte, 52)}, {Name: "RqLs", Data: make([]byte, 52)}}},
		{"short DH2Q", []wire.CreateContext{{Name: "DH2Q", Data: []byte{1}}}},
		{"short DH2C", []wire.CreateContext{{Name: "DH2C", Data: []byte{1}}}},
		{"two DH2Q", []wire.CreateContext{durable, durable}},
		{"DH2Q and DH2C", []wire.CreateContext{durable, reconnect}},
		{"DH2Q and DHnQ", []wire.CreateContext{{Name: "DHnQ", Data: make([]byte, 16)}, durable}},
		{"no CREATE GUID", []wire.CreateContext{{Name: "DH2Q", Data: make([]byte, 32)}}},
	} {
		request := wire.CreateRequest{Name: "must-not-exist", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf, Contexts: test.contexts}
		if _, status := rawCreate(t, client, request); status != smb.StatusInvalidParameter {
			t.Fatalf("%s: status %#x", test.name, status)
		}
		if srv.exists(t, request.Name) {
			t.Fatalf("%s: CREATE made the file", test.name)
		}
	}
}

// A CREATE GUID in use is refused before anything changes, also for a marked
// replay: without multichannel macOS does not replay a CREATE.
func TestDuplicateCreateGUIDChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	options := durableCreate("file", 1)
	options.Request.Disposition = fileOverwriteIf
	created := mustCreate(t, client, options)
	data := []byte("acknowledged data")
	if status := client.write(t, wire.WriteRequest{ID: created.Reply.ID, Data: data}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	replay := options
	replay.Replay = true
	other := options
	other.Request.Name = "other"
	for _, duplicate := range []smbtest.CreateOptions{replay, other} {
		if _, status := client.create(t, duplicate); status != smb.StatusDuplicateObjectID {
			t.Fatalf("duplicate CREATE of %q: status %#x", duplicate.Request.Name, status)
		}
	}
	if got, status := client.read(t, wire.ReadRequest{ID: created.Reply.ID, Length: 64}); status != smb.StatusSuccess || !bytes.Equal(got, data) {
		t.Fatalf("READ after duplicates = %q, %#x", got, status)
	}
	if srv.exists(t, "other") {
		t.Fatal("duplicate CREATE made a file")
	}
}

// The Mac can come back on a new connection before the server notices that
// the old one died. Its new session replaces the old one, which detaches the
// durable open, and DH2C hands it back with its lease and byte ranges. The reply
// has no DH2Q: macOS fails the reconnect when it gets one.
func TestReconnectWhileOldConnectionLives(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	options := durableCreate("file", 1)
	options.Lease.Flags, options.Lease.ParentKey = leaseParentKeySet, [16]byte{8}
	created := mustCreate(t, client, options)
	lock := wire.LockRequest{ID: created.Reply.ID, Elements: []wire.LockElement{{Length: 10, Flags: 2}}}
	if status := client.lock(t, lock); status != smb.StatusSuccess {
		t.Fatalf("LOCK status %#x", status)
	}
	resumed := client.reconnect(t)
	reopened := mustCreate(t, resumed, reclaimCreate(options, created))
	id := reopened.Reply.ID
	if id.Persistent != created.Reply.ID.Persistent || id.Volatile == created.Reply.ID.Volatile || reopened.Reply.Action != 1 ||
		reopened.Durable != nil || *reopened.Lease != *created.Lease {
		t.Fatalf("DH2C = %+v, lease %+v; created %+v, lease %+v", reopened.Reply, reopened.Lease, created.Reply, created.Lease)
	}
	if _, status := resumed.read(t, wire.ReadRequest{ID: created.Reply.ID, Length: 1}); status != smb.StatusFileClosed {
		t.Fatalf("READ on the old volatile ID: status %#x", status)
	}
	lock.ID, lock.Elements[0].Flags = id, 4
	if status := resumed.lock(t, lock); status != smb.StatusSuccess {
		t.Fatalf("unlock of the kept range: status %#x", status)
	}
}

func TestReconnectRefusesMismatches(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	options := durableCreate("file", 1)
	created := mustCreate(t, client, options)
	good := reclaimCreate(options, created)
	if _, status := client.create(t, good); status != smb.StatusObjectNameNotFound {
		t.Fatalf("DH2C of an attached open: status %#x", status)
	}
	client.drop(t)
	resumed := client.reconnect(t)
	for _, test := range []struct {
		modify func(*smbtest.CreateOptions)
		name   string
		want   smb.Status
	}{
		{modify: func(o *smbtest.CreateOptions) { o.Reconnect.ID.Volatile++ }, name: "file ID", want: smb.StatusObjectNameNotFound},
		{modify: func(o *smbtest.CreateOptions) { o.Reconnect.CreateGUID[0]++ }, name: "CREATE GUID", want: smb.StatusObjectNameNotFound},
		{modify: func(o *smbtest.CreateOptions) { o.Lease.Key[0]++ }, name: "lease key", want: smb.StatusObjectNameNotFound},
		{modify: func(o *smbtest.CreateOptions) { o.Lease = nil }, name: "no lease", want: smb.StatusObjectNameNotFound},
		{modify: func(o *smbtest.CreateOptions) {
			o.Reconnect = nil
			o.Request.Contexts = []wire.CreateContext{{Name: "DHnC", Data: make([]byte, 16)}}
		}, name: "durable v1", want: smb.StatusObjectNameNotFound},
		{modify: func(o *smbtest.CreateOptions) { o.Reconnect.Flags = 2 }, name: "persistent", want: smb.StatusInvalidParameter},
	} {
		bad := good
		lease, reconnect := *good.Lease, *good.Reconnect
		bad.Lease, bad.Reconnect = &lease, &reconnect
		test.modify(&bad)
		bad.Request.Name = "must-not-exist"
		if _, status := resumed.create(t, bad); status != test.want {
			t.Fatalf("%s: status %#x, want %#x", test.name, status, test.want)
		}
		if srv.exists(t, bad.Request.Name) {
			t.Fatalf("%s: refused DH2C made a file", test.name)
		}
	}
	mustCreate(t, resumed, good)
}

// DH2C goes by the file, not its old name: a renamed file is reclaimed by
// its new name, and the old name finds nothing, even when reused.
func TestReconnectFollowsRename(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace bool
	}{{"old name free", false}, {"old name reused", true}} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			options := durableCreate("file", 1)
			created := mustCreate(t, client, options)
			client.drop(t)
			source, err := srv.adapter.Lookup(t.Context(), "file")
			if err != nil {
				t.Fatal(err)
			}
			destination, err := srv.adapter.Lookup(t.Context(), "renamed")
			if err != nil {
				t.Fatal(err)
			}
			if err = srv.adapter.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: destination.Name, SourceInode: source.Object.Inode}); err != nil {
				t.Fatal(err)
			}
			if test.replace {
				if _, err = srv.adapter.Create(t.Context(), source.Name, smb.KindFile); err != nil {
					t.Fatal(err)
				}
			}
			resumed := client.reconnect(t)
			reconnect := reclaimCreate(options, created)
			if _, status := resumed.create(t, reconnect); status != smb.StatusObjectNameNotFound {
				t.Fatalf("DH2C by the old name: status %#x", status)
			}
			reconnect.Request.Name = "renamed"
			if reopened := mustCreate(t, resumed, reconnect); reopened.Reply.ID.Persistent != created.Reply.ID.Persistent {
				t.Fatalf("DH2C by the new name = %+v", reopened.Reply)
			}
		})
	}
}

// A file being deleted on close has no name to check; reclaiming it keeps
// the deletion.
func TestReconnectKeepsDeleteOnClose(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	options := durableCreate("file", 1)
	options.Request.Options = fileDeleteOnClose
	created := mustCreate(t, client, options)
	client.drop(t)
	resumed := client.reconnect(t)
	reconnect := reclaimCreate(options, created)
	reconnect.Request.Name = "another-name"
	reopened := mustCreate(t, resumed, reconnect)
	if status := resumed.close(t, reopened.Reply.ID); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
	if srv.exists(t, "file") || srv.exists(t, "another-name") {
		t.Fatal("delete on close did not remove the file")
	}
}

// A detached open lives exactly its durable timeout. Expiry closes it like a
// CLOSE: its ranges go, acknowledged data stays, a pending delete happens.
func TestDurableOpenExpiresAfterNetworkCut(t *testing.T) {
	for _, test := range []struct {
		name    string
		options uint32
	}{{"kept file", 0}, {"delete on close", fileDeleteOnClose}} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			options := durableCreate("file", 1)
			options.Request.Options = test.options
			created := mustCreate(t, client, options)
			data := []byte("acknowledged data")
			if status := client.write(t, wire.WriteRequest{ID: created.Reply.ID, Data: data}); status != smb.StatusSuccess {
				t.Fatalf("WRITE status %#x", status)
			}
			if status := client.lock(t, wire.LockRequest{ID: created.Reply.ID, Elements: []wire.LockElement{{Length: 17, Flags: 2}}}); status != smb.StatusSuccess {
				t.Fatalf("LOCK status %#x", status)
			}
			client.drop(t)
			srv.clock.advance(smb.DefaultDurableTimeout - time.Nanosecond)
			srv.expire(t)
			if !srv.exists(t, "file") {
				t.Fatal("file deleted before the durable timeout")
			}
			srv.clock.advance(time.Nanosecond)
			srv.expire(t)
			resumed := client.reconnect(t)
			if _, status := resumed.create(t, reclaimCreate(options, created)); status != smb.StatusObjectNameNotFound {
				t.Fatalf("DH2C after expiry: status %#x", status)
			}
			if test.options == fileDeleteOnClose {
				if srv.exists(t, "file") {
					t.Fatal("expiry kept a delete-on-close file")
				}
				return
			}
			peer := srv.connect(t)
			id := peer.open(t, "file")
			if status := peer.lock(t, wire.LockRequest{ID: id, Elements: []wire.LockElement{{Length: 17, Flags: 2}}}); status != smb.StatusSuccess {
				t.Fatalf("LOCK after expiry: status %#x", status)
			}
			if got, status := peer.read(t, wire.ReadRequest{ID: id, Length: 64}); status != smb.StatusSuccess || !bytes.Equal(got, data) {
				t.Fatalf("READ after expiry = %q, %#x", got, status)
			}
		})
	}
}

// Nobody can answer a break for a detached open, so an open that needs its H
// lease gone closes it at once instead of waiting for the timeout.
func TestDetachedDurableOpenClosesForHandleBreak(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	options := durableCreate("file", 1)
	options.Request.ShareAccess = 1
	created := mustCreate(t, client, options)
	data := []byte("acknowledged data")
	if status := client.write(t, wire.WriteRequest{ID: created.Reply.ID, Data: data}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	client.drop(t)
	peer := srv.connect(t)
	id := peer.open(t, "file")
	if got, status := peer.read(t, wire.ReadRequest{ID: id, Length: 64}); status != smb.StatusSuccess || !bytes.Equal(got, data) {
		t.Fatalf("READ = %q, %#x", got, status)
	}
	if _, status := client.reconnect(t).create(t, reclaimCreate(options, created)); status != smb.StatusObjectNameNotFound {
		t.Fatalf("DH2C after the break: status %#x", status)
	}
}

// While a break takes H away, a new open on the same lease sees the break in
// its lease context and gets no durable handle.
func TestNoDurableHandleWhileHandleLeaseIsBroken(t *testing.T) {
	srv := newTestServer(t)
	holder, opener := srv.connect(t), srv.connect(t)
	options := durableCreate("file", 1)
	options.Request.ShareAccess = 1
	mustCreate(t, holder, options)
	request := opener.sendCreate(t, writerCreate(fileOpen))
	notification := holder.leaseBreak(t)
	if notification.NewState != leaseR {
		t.Fatalf("break = %+v", notification)
	}
	opener.interim(t, request)
	again := durableCreate("file", 1)
	again.Durable.CreateGUID = [16]byte{2}
	again.Request.DesiredAccess, again.Request.ShareAccess = fileReadData, 7
	result := mustCreate(t, holder, again)
	if result.Durable != nil || result.Lease.State != leaseRH || result.Lease.Flags != leaseBreakInProgress {
		t.Fatalf("open during the break: lease %+v, durable %+v", result.Lease, result.Durable)
	}
	if status := holder.ackLease(t, notification.Key, leaseR); status != smb.StatusSuccess {
		t.Fatalf("lease ACK status %#x", status)
	}
	if _, status := opener.created(t, request); status != smb.StatusSharingViolation {
		t.Fatalf("CREATE status %#x", status)
	}
}

// A network cut during READ, WRITE or FLUSH keeps acknowledged data, the
// open's sharing and its byte ranges, and the Mac reclaims the open.
func TestDurableReconnectDuringIO(t *testing.T) {
	for _, protection := range []struct {
		name   string
		cipher uint16
	}{{"signed", 0}, {"encrypted", smb.CipherAES256GCM}} {
		for _, operation := range []struct {
			name    string
			command wire.Command
		}{{"write", wire.Write}, {"read", wire.Read}, {"flush", wire.Flush}} {
			t.Run(protection.name+"/"+operation.name, func(t *testing.T) {
				checkDurableReconnectDuringIO(t, protection.cipher, operation.command)
			})
		}
	}
}

func checkDurableReconnectDuringIO(t *testing.T, cipher uint16, command wire.Command) {
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
