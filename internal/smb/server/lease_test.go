package server

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	leaseR   = smb.LeaseRead
	leaseRH  = smb.LeaseRead | smb.LeaseHandle
	leaseRWH = smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle
)

// leasedCreate opens or creates name for reading and writing, sharing
// everything, with a V2 lease request.
func leasedCreate(name string, key byte, leaseState uint32) smbtest.CreateOptions {
	return smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: name, DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileOpenIf},
		Lease:   &wire.LeaseContext{Version: 2, Key: [16]byte{key}, State: leaseState},
	}
}

// writerCreate opens "file" for writing without a lease.
func writerCreate(disposition uint32) smbtest.CreateOptions {
	return smbtest.CreateOptions{Request: wire.CreateRequest{Name: "file", DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: disposition}}
}

func mustCreate(t *testing.T, client *testClient, options smbtest.CreateOptions) smbtest.CreateResult {
	t.Helper()
	result, status := client.create(t, options)
	if status != smb.StatusSuccess {
		t.Fatalf("CREATE %q: status %#x", options.Request.Name, status)
	}
	return result
}

// rawCreate sends request with its contexts exactly as given.
func rawCreate(t *testing.T, client *testClient, request wire.CreateRequest) (smbtest.CreateResult, smb.Status) {
	t.Helper()
	return decodeReply(t, client.call(t, wire.Create, encode(t, wire.EncodeCreateRequest, request), 1), smbtest.DecodeCreateReply)
}

func (s *testServer) object(t *testing.T, name string) smb.Inode {
	t.Helper()
	resolved, err := s.storage.Lookup(t.Context(), name)
	if err != nil || !resolved.Exists {
		t.Fatalf("lookup %q: %+v, %v", name, resolved, err)
	}
	return resolved.Object
}

func TestLeaseGrantedStates(t *testing.T) {
	client := newTestServer(t).connect(t)
	for _, test := range []struct {
		name            string
		requested, want uint32
	}{
		{"R", leaseR, leaseR},
		{"RH", leaseRH, leaseRH},
		{"RWH", leaseRWH, leaseRWH},
		{"RW falls back to R", smb.LeaseRead | smb.LeaseWrite, leaseR},
		{"H alone is declined", smb.LeaseHandle, 0},
		{"WH is declined", smb.LeaseWrite | smb.LeaseHandle, 0},
		{"none", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := leasedCreate(test.name, 1, test.requested)
			options.Lease.Epoch, options.Lease.Flags, options.Lease.ParentKey = 7, leaseParentKeySet, [16]byte{8}
			result := mustCreate(t, client, options)
			wantEpoch, wantLevel := uint16(8), leaseOplockLevel
			if test.want == 0 {
				wantEpoch, wantLevel = 7, 0
			}
			lease := result.Lease
			if lease == nil || lease.State != test.want || lease.Epoch != wantEpoch || lease.ParentKey != [16]byte{8} || lease.Flags != leaseParentKeySet || result.Reply.OplockLevel != wantLevel {
				t.Fatalf("lease = %+v, oplock level %#x; want state %#x, epoch %d", lease, result.Reply.OplockLevel, test.want, wantEpoch)
			}
			if status := client.close(t, result.Reply.ID); status != smb.StatusSuccess {
				t.Fatalf("CLOSE status %#x", status)
			}
		})
	}
}

func TestLeaseOnlyOnePerFile(t *testing.T) {
	client := newTestServer(t).connect(t)
	plain := client.open(t, "file")
	first := mustCreate(t, client, leasedCreate("file", 1, leaseRWH))
	if first.Lease.State != leaseRH || first.Lease.Epoch != 1 {
		t.Fatalf("lease beside another open = %+v, want RH", first.Lease)
	}
	if status := client.close(t, plain); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
	upgraded := mustCreate(t, client, leasedCreate("file", 1, leaseRWH))
	if upgraded.Lease.State != leaseRWH || upgraded.Lease.Epoch != 2 {
		t.Fatalf("upgraded lease = %+v, want RWH at epoch 2", upgraded.Lease)
	}
	if joined := mustCreate(t, client, leasedCreate("file", 1, leaseR)); joined.Lease.State != leaseRWH || joined.Lease.Epoch != 2 {
		t.Fatalf("a smaller request changed the lease: %+v", joined.Lease)
	}
	// Only reading attributes, so that the open breaks nothing.
	attributes := leasedCreate("file", 2, leaseRWH)
	attributes.Request.DesiredAccess = 0x80
	second := mustCreate(t, client, attributes)
	if second.Lease.State != 0 || second.Reply.OplockLevel != 0 {
		t.Fatalf("second lease key on the file = %+v", second.Lease)
	}
	if _, status := client.create(t, leasedCreate("other", 1, leaseRH)); status != smb.StatusInvalidParameter {
		t.Fatalf("lease key moved to another file: status %#x", status)
	}
}

// A lease key that names another file fails the CREATE before it changes
// anything.
func TestLeaseKeyOfAnotherFileChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	mustCreate(t, client, leasedCreate("a", 1, leaseRH))
	data := client.open(t, "b")
	if status := client.write(t, wire.WriteRequest{ID: data, Data: []byte("data")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	overwrite := leasedCreate("b", 1, leaseRH)
	overwrite.Request.Disposition = fileOverwriteIf
	if _, status := client.create(t, overwrite); status != smb.StatusInvalidParameter {
		t.Fatalf("OVERWRITE_IF with the key of another file: status %#x", status)
	}
	if attr, err := srv.storage.GetAttr(t.Context(), srv.object(t, "b")); err != nil || attr.Size != 4 {
		t.Fatalf("size after the refused overwrite = %d, %v", attr.Size, err)
	}
	created := leasedCreate("c", 1, leaseRH)
	created.Request.Disposition = fileCreateDisposition
	if _, status := client.create(t, created); status != smb.StatusInvalidParameter {
		t.Fatalf("FILE_CREATE with the key of another file: status %#x", status)
	}
	if resolved, err := srv.storage.Lookup(t.Context(), "c"); err != nil || resolved.Exists {
		t.Fatalf("refused FILE_CREATE left %+v, %v", resolved, err)
	}
}

func TestNoLeaseForDirectoriesAndOplocks(t *testing.T) {
	client := newTestServer(t).connect(t)
	directory := leasedCreate("dir", 1, leaseRWH)
	directory.Request.Options = fileDirectoryFile
	oplock := smbtest.CreateOptions{Request: wire.CreateRequest{Name: "oplock", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpenIf, OplockLevel: 8}}
	for _, options := range []smbtest.CreateOptions{directory, oplock} {
		if result := mustCreate(t, client, options); result.Lease != nil || result.Reply.OplockLevel != 0 {
			t.Fatalf("%q got caching: %+v, oplock level %#x", options.Request.Name, result.Lease, result.Reply.OplockLevel)
		}
	}
	v1 := createContext(t, wire.EncodeLeaseContext, wire.LeaseContext{Version: 1, Key: [16]byte{3}, State: leaseRWH})
	result, status := rawCreate(t, client, wire.CreateRequest{Name: "v1", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpenIf, OplockLevel: leaseOplockLevel, Contexts: []wire.CreateContext{v1}})
	if status != smb.StatusSuccess || result.Lease != nil || result.Reply.OplockLevel != 0 {
		t.Fatalf("V1 lease = %+v, %#x", result, status)
	}
}

// Another open waits for the lease holder to give up what it takes away, so
// the holder's cached writes reach the file before the open changes it.
func TestConflictingOpenBreaksLease(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition uint32
		target      uint32
		size        uint64
	}{
		{name: "write", disposition: fileOpen, target: leaseRH, size: 17},
		{name: "overwrite", disposition: fileOverwrite, target: 0, size: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			holder, opener := srv.connect(t), srv.connect(t)
			held := mustCreate(t, holder, leasedCreate("file", 1, leaseRWH))
			if status := holder.write(t, wire.WriteRequest{ID: held.Reply.ID, Data: []byte("seventeen bytes!!")}); status != smb.StatusSuccess {
				t.Fatalf("WRITE status %#x", status)
			}
			request := opener.sendCreate(t, writerCreate(test.disposition))
			notification := holder.leaseBreak(t)
			want := wire.LeaseBreakNotification{Key: [16]byte{1}, Epoch: 2, Flags: 1, CurrentState: leaseRWH, NewState: test.target}
			if notification != want {
				t.Fatalf("break = %+v, want %+v", notification, want)
			}
			opener.interim(t, request)
			if attr, err := srv.storage.GetAttr(t.Context(), srv.object(t, "file")); err != nil || attr.Size != 17 {
				t.Fatalf("size before ACK = %d, %v", attr.Size, err)
			}
			if status := holder.ackLease(t, notification.Key, notification.NewState); status != smb.StatusSuccess {
				t.Fatalf("lease ACK status %#x", status)
			}
			opened, status := opener.created(t, request)
			if status != smb.StatusSuccess || opened.Reply.Size != test.size {
				t.Fatalf("CREATE after ACK = %+v, %#x", opened.Reply, status)
			}
		})
	}
}

func TestAttributesOnlyOpenKeepsLease(t *testing.T) {
	srv := newTestServer(t)
	holder, opener := srv.connect(t), srv.connect(t)
	mustCreate(t, holder, leasedCreate("file", 1, leaseRWH))
	mustCreate(t, opener, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "file", DesiredAccess: 0x80, ShareAccess: 7, Disposition: fileOpen}})
	again := mustCreate(t, holder, leasedCreate("file", 1, leaseRWH))
	if again.Lease.State != leaseRWH || again.Lease.Epoch != 1 || again.Lease.Flags != 0 {
		t.Fatalf("lease after an attributes-only open = %+v", again.Lease)
	}
}

func TestReadLeaseBreakNeedsNoAcknowledgment(t *testing.T) {
	srv := newTestServer(t)
	holder, opener := srv.connect(t), srv.connect(t)
	mustCreate(t, holder, leasedCreate("file", 1, leaseR))
	request := opener.sendCreate(t, writerCreate(fileOverwrite))
	if notification := holder.leaseBreak(t); notification.CurrentState != leaseR || notification.NewState != 0 || notification.Flags != 0 {
		t.Fatalf("break = %+v", notification)
	}
	if _, status := opener.created(t, request); status != smb.StatusSuccess {
		t.Fatalf("CREATE status %#x", status)
	}
}

// An open that conflicts with the sharing of a cached handle breaks H, so
// the client can close the handle, and checks sharing once more. A Mac does
// this on one connection, which must answer the CLOSE or ACK while the CREATE
// waits.
func TestSharingConflictBreaksHandleLease(t *testing.T) {
	for _, test := range []struct {
		name  string
		lease uint32
		close bool
		want  smb.Status
	}{
		{name: "holder closes", lease: leaseRH, close: true, want: smb.StatusSuccess},
		{name: "holder keeps the handle", lease: leaseRH, want: smb.StatusSharingViolation},
		{name: "no H", lease: leaseR, want: smb.StatusSharingViolation},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestServer(t).connect(t)
			options := leasedCreate("file", 1, test.lease)
			options.Request.ShareAccess = 1
			held := mustCreate(t, client, options)
			request := client.sendCreate(t, writerCreate(fileOpen))
			if test.lease == leaseRH {
				notification := client.leaseBreak(t)
				if notification.CurrentState != leaseRH || notification.NewState != leaseR || notification.Flags != 1 {
					t.Fatalf("break = %+v", notification)
				}
				client.interim(t, request)
				status := smb.Status(0)
				if test.close {
					status = client.close(t, held.Reply.ID)
				} else {
					status = client.ackLease(t, notification.Key, leaseR)
				}
				if status != smb.StatusSuccess {
					t.Fatalf("holder status %#x", status)
				}
			}
			if _, status := client.created(t, request); status != test.want {
				t.Fatalf("CREATE status %#x, want %#x", status, test.want)
			}
		})
	}
}

// A conflicting open under the opener's own lease breaks nothing: the client
// knows its own cached handles.
func TestSharingConflictKeepsOpenersLease(t *testing.T) {
	client := newTestServer(t).connect(t)
	options := leasedCreate("file", 1, leaseRH)
	options.Request.ShareAccess = 1
	mustCreate(t, client, options)
	writer := writerCreate(fileOpen)
	writer.Lease = options.Lease
	if _, status := client.create(t, writer); status != smb.StatusSharingViolation {
		t.Fatalf("CREATE status %#x", status)
	}
	reader := leasedCreate("file", 1, leaseRH)
	reader.Request.DesiredAccess = fileReadData
	if lease := mustCreate(t, client, reader).Lease; lease == nil || lease.State != leaseRH {
		t.Fatalf("lease after the conflict = %+v", lease)
	}
}

func TestLeaseBreakNotificationProtection(t *testing.T) {
	for _, test := range []struct {
		name       string
		cipher     uint16
		signing    uint16
		encryption EncryptionPolicy
	}{
		{"signed", 0, smb.SigningCMAC, AllowPlaintext},
		{"GCM negotiated", smb.CipherAES128GCM, smb.SigningGMAC, AllowPlaintext},
		{"encrypted", smb.CipherAES256GCM, smb.SigningGMAC, RequireEncryption},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.server.options.Encryption = test.encryption
			holder := srv.dial(t, smbtest.LoginOptions{Cipher: test.cipher, Signing: test.signing})
			opener := srv.connect(t)
			mustCreate(t, holder, leasedCreate("file", 1, leaseRWH))
			request := opener.sendCreate(t, writerCreate(fileOpen))
			want := wire.LeaseBreakNotification{Key: [16]byte{1}, Epoch: 2, Flags: 1, CurrentState: leaseRWH, NewState: leaseRH}
			if notification := holder.leaseBreak(t); notification != want {
				t.Fatalf("break = %+v, want %+v", notification, want)
			}
			if status := holder.ackLease(t, want.Key, leaseRH); status != smb.StatusSuccess {
				t.Fatalf("lease ACK status %#x", status)
			}
			if _, status := opener.created(t, request); status != smb.StatusSuccess {
				t.Fatalf("CREATE status %#x", status)
			}
		})
	}
}

func TestLeaseAcknowledgmentErrors(t *testing.T) {
	srv := newTestServer(t)
	holder, opener := srv.connect(t), srv.connect(t)
	held := mustCreate(t, holder, leasedCreate("file", 1, leaseRWH))
	request := opener.sendCreate(t, writerCreate(fileOpen))
	notification := holder.leaseBreak(t)
	for _, test := range []struct {
		client *testClient
		name   string
		key    byte
		state  uint32
		want   smb.Status
	}{
		{holder, "unknown key", 9, leaseR, smb.StatusObjectNameNotFound},
		{holder, "more than the break leaves", 1, leaseRWH, smb.StatusRequestNotAccepted},
		{holder, "valid", 1, leaseRH, smb.StatusSuccess},
		{holder, "no break pending", 1, leaseRH, smb.StatusUnsuccessful},
	} {
		if status := test.client.ackLease(t, [16]byte{test.key}, test.state); status != test.want {
			t.Fatalf("%s: lease ACK status %#x, want %#x (break %+v)", test.name, status, test.want, notification)
		}
	}
	if _, status := opener.created(t, request); status != smb.StatusSuccess {
		t.Fatalf("CREATE status %#x", status)
	}
	body := encode(t, wire.EncodeOplockBreakRequest, wire.OplockBreakRequest{ID: held.Reply.ID})
	if status := holder.call(t, wire.OplockBreak, body, 1).Header.Status; status != smb.StatusInvalidDeviceState {
		t.Fatalf("oplock ACK status %#x", status)
	}
}

// A holder that never acknowledges loses the whole lease after the timeout,
// and with it durability, but keeps its open.
func TestLeaseBreakTimeoutRevokesLease(t *testing.T) {
	srv := newTestServer(t)
	holder, opener := srv.connect(t), srv.connect(t)
	options := leasedCreate("file", 1, leaseRWH)
	options.Durable = &wire.DurableRequest{CreateGUID: [16]byte{1}}
	held := mustCreate(t, holder, options)
	request := opener.sendCreate(t, writerCreate(fileOpen))
	holder.leaseBreak(t)
	opener.interim(t, request)
	object := srv.object(t, "file")
	srv.clock.advance(state.LeaseBreakTimeout - time.Nanosecond)
	srv.expire(t)
	if !srv.server.options.State.LeaseBreaking(object, state.GUID{}, state.GUID{}) {
		t.Fatal("break ended before its timeout")
	}
	srv.clock.advance(time.Nanosecond)
	srv.expire(t)
	if _, status := opener.created(t, request); status != smb.StatusSuccess {
		t.Fatalf("CREATE after the timeout: status %#x", status)
	}
	if status := holder.ackLease(t, [16]byte{1}, leaseRH); status != smb.StatusUnsuccessful {
		t.Fatalf("late lease ACK status %#x", status)
	}
	open, status := srv.server.options.State.Find(state.FileID(held.Reply.ID), state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID})
	if status != smb.StatusSuccess || open.Durable {
		t.Fatalf("holder's open after the timeout = %+v, %#x", open, status)
	}
	if status := holder.write(t, wire.WriteRequest{ID: held.Reply.ID, Data: []byte("still open")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE after the timeout: status %#x", status)
	}
}
