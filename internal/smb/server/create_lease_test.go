package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateLeaseV2SupportedStates(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	for _, requested := range []uint32{0, 1, 2, 3, 4, 5, 6, 7, 9} {
		name := fmt.Sprintf("state-%d", requested)
		result := first.create(t, leaseCreateRequest(name), leaseV2(1, requested))
		want := requested & 7
		if want&smb.LeaseRead == 0 {
			want = 0
		} else if want&smb.LeaseHandle == 0 {
			want &^= smb.LeaseWrite
		}
		assertLeaseGrant(t, result, want)
		epoch := uint16(7)
		if want != 0 {
			epoch++
		}
		if result.Lease.Epoch != epoch || result.Lease.ParentKey != [16]byte{8} || result.Lease.Flags != leaseParentKeySet {
			t.Fatalf("V2 fields = %+v", result.Lease)
		}
		first.close(t, result.Reply.ID)
		observer := second.create(t, leaseCreateRequest(name), nil)
		if observer.Reply.OplockLevel != 0 || observer.Lease != nil {
			t.Fatalf("unleased observer = %+v", observer)
		}
		second.close(t, observer.Reply.ID)
	}
}

func TestCreateLeaseSafeSubsetWithOtherOpens(t *testing.T) {
	for _, access := range []uint32{fileReadData, fileWriteData, 0x80} {
		t.Run(fmt.Sprintf("access-%x", access), func(t *testing.T) {
			_, first, second := newCreateLeaseClients(t)
			other := leaseCreateRequest("file")
			other.DesiredAccess = access
			opened := first.create(t, other, nil)
			result := second.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
			assertLeaseGrant(t, result, smb.LeaseRead|smb.LeaseHandle)
			first.close(t, opened.Reply.ID)
			second.close(t, result.Reply.ID)
		})
	}
}

func TestCreateLeaseSharesStateEpochAndParent(t *testing.T) {
	server, first, observer := newCreateLeaseClients(t)
	second := loginCreateLeaseClient(t, server, 2)
	for i, requested := range []uint32{1, 3, 7, 1} {
		client := first
		if i%2 != 0 {
			client = second
		}
		lease := leaseV2(1, requested)
		if i != 0 {
			lease.Epoch, lease.ParentKey = 999, [16]byte{9}
		}
		result := client.create(t, leaseCreateRequest("file"), lease)
		want, epoch := requested, uint16(8+i)
		if i == 3 {
			want, epoch = 7, 10
		}
		assertLeaseGrant(t, result, want)
		if result.Lease.Epoch != epoch || result.Lease.ParentKey != [16]byte{8} {
			t.Fatalf("shared lease = %+v", result.Lease)
		}
	}
	id := observer.send(t, leaseCreateRequest("file"), nil)
	notification, err := first.client.WaitLeaseBreak(first.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.Epoch != 11 || notification.CurrentState != 7 || notification.NewState != 3 {
		t.Fatalf("shared break = %+v", notification)
	}
	// An open on another session of the same client and key can acknowledge.
	second.ack(t, notification)
	observer.created(t, id)
}

func TestCreateBreaksConflictingLeasesBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition uint32
		access      uint32
		target      uint32
	}{
		{name: "read", disposition: fileOpen, access: fileReadData, target: 3},
		{name: "metadata", disposition: fileOpen, access: 0x80, target: 7},
		{name: "write", disposition: fileOpen, access: fileWriteData, target: 3},
		{name: "append", disposition: fileOpen, access: fileAppendData, target: 3},
		{name: "overwrite", disposition: fileOverwrite, access: fileWriteData},
		{name: "overwrite-if", disposition: fileOverwriteIf, access: fileWriteData},
		{name: "supersede", disposition: fileSupersede, access: fileDelete},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, first, second := newCreateLeaseClients(t)
			opened := first.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
			resolved, err := server.options.Storage.Lookup(first.ctx, "file")
			if err != nil {
				t.Fatal(err)
			}
			size := uint64(17)
			if setErr := server.options.Storage.SetAttr(first.ctx, resolved.Object, smb.AttrChange{Size: &size}); setErr != nil {
				t.Fatal(setErr)
			}
			other := leaseCreateRequest("file")
			other.Disposition, other.DesiredAccess = test.disposition, test.access
			id := second.send(t, other, nil)
			if test.target == 7 {
				metadata := second.created(t, id)
				shared := first.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
				assertLeaseGrant(t, shared, 7)
				if shared.Lease.Epoch != 8 || shared.Lease.Flags != leaseParentKeySet || metadata.Reply.Size != size {
					t.Fatalf("metadata-only OPEN changed caching or bytes: %+v, size %d", shared.Lease, metadata.Reply.Size)
				}
				first.close(t, opened.Reply.ID)
				first.close(t, shared.Reply.ID)
				second.close(t, metadata.Reply.ID)
				return
			}
			notification, err := first.client.WaitLeaseBreak(first.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if notification.Key != [16]byte{1} || notification.CurrentState != 7 || notification.NewState != test.target || notification.Epoch != 9 || notification.Flags != 1 {
				t.Fatalf("break = %+v", notification)
			}
			done := receiveCreateLater(second, id)
			assertCreateWaits(t, done)
			attr, err := server.options.Storage.GetAttr(first.ctx, resolved.Object)
			if err != nil || attr.Size != size {
				t.Fatalf("mutation before ACK: size %d, error %v", attr.Size, err)
			}
			first.ack(t, notification)
			result := finishCreate(t, done)
			wantSize := size
			if test.disposition != fileOpen {
				wantSize = 0
			}
			if result.Reply.Size != wantSize {
				t.Fatalf("size = %d, want %d", result.Reply.Size, wantSize)
			}
			first.close(t, opened.Reply.ID)
			second.close(t, result.Reply.ID)
		})
	}
}

func TestCreateReadLeaseBreakNeedsNoAcknowledgment(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	first.create(t, leaseCreateRequest("file"), leaseV2(1, 1))
	other := leaseCreateRequest("file")
	other.Disposition, other.DesiredAccess = fileOverwrite, fileWriteData
	id := second.send(t, other, nil)
	notification, err := first.client.WaitLeaseBreak(first.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.CurrentState != 1 || notification.NewState != 0 || notification.Flags != 0 {
		t.Fatalf("R break = %+v", notification)
	}
	second.created(t, id)
}

func TestCreateSharingViolationBreaksHandleAndRetriesOnce(t *testing.T) {
	for _, closeHolder := range []bool{false, true} {
		t.Run(fmt.Sprintf("close-%v", closeHolder), func(t *testing.T) {
			_, first, second := newCreateLeaseClients(t)
			held := leaseCreateRequest("file")
			held.ShareAccess = 1
			opened := first.create(t, held, leaseV2(1, 3))
			other := leaseCreateRequest("file")
			other.DesiredAccess = fileWriteData
			id := second.send(t, other, nil)
			notification, err := first.client.WaitLeaseBreak(first.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if notification.CurrentState != 3 || notification.NewState != smb.LeaseRead || notification.Flags != 1 {
				t.Fatalf("sharing break = %+v", notification)
			}
			done := receiveCreateLater(second, id)
			assertCreateWaits(t, done)
			if closeHolder {
				// CLOSE must acquire the same parent guard that CREATE released.
				first.close(t, opened.Reply.ID)
				finishCreate(t, done)
			} else {
				first.ack(t, notification)
				result := <-done
				if result.err != nil || result.message.Header.Status != smb.StatusSharingViolation {
					t.Fatalf("second sharing check = %#x, error %v", result.message.Header.Status, result.err)
				}
			}
		})
	}
}

func TestCreateSharingViolationWithoutHandleLeaseFailsAtOnce(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	held := leaseCreateRequest("file")
	held.ShareAccess = 1
	first.create(t, held, leaseV2(1, 1))
	other := leaseCreateRequest("file")
	other.DesiredAccess = fileWriteData
	id := second.send(t, other, nil)
	message, err := second.receive(id)
	if err != nil || message.Header.Status != smb.StatusSharingViolation {
		t.Fatalf("sharing check = %#x, error %v", message.Header.Status, err)
	}
}

func TestCreateRefusesClassicOplocksAndNonFileLeases(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	for _, level := range []uint8{1, 8, 9} {
		request := leaseCreateRequest(fmt.Sprintf("classic-%d", level))
		request.OplockLevel = level
		for _, client := range []*createLeaseClient{first, second} {
			result := client.create(t, request, nil)
			if result.Reply.OplockLevel != 0 || result.Lease != nil {
				t.Fatalf("classic oplock grant = %+v", result)
			}
		}
	}
	directory := leaseCreateRequest("dir")
	directory.Options = fileDirectoryFile
	result := first.create(t, directory, leaseV2(10, 7))
	if result.Reply.OplockLevel != 0 || result.Lease != nil {
		t.Fatalf("directory lease = %+v", result)
	}
	first.create(t, leaseCreateRequest("base"), nil)
	result = second.create(t, leaseCreateRequest("base:meta:$DATA"), leaseV2(11, 7))
	if result.Reply.OplockLevel != 0 || result.Lease != nil {
		t.Fatalf("stream lease = %+v", result)
	}
	lease, err := wire.EncodeLeaseContext(wire.LeaseContext{Version: 1, Key: [16]byte{12}, State: 7})
	if err != nil {
		t.Fatal(err)
	}
	v1 := leaseCreateRequest("v1")
	v1.OplockLevel, v1.Contexts = leaseOplockLevel, []wire.CreateContext{lease}
	message, err := first.receive(first.sendRawCreate(t, v1))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := smbtest.DecodeCreateReply(message)
	if err != nil || decoded.Reply.OplockLevel != 0 || decoded.Lease != nil {
		t.Fatalf("V1 lease = %+v, error %v", decoded, err)
	}
}

func TestCreateLeaseContextValidation(t *testing.T) {
	server, first, _ := newCreateLeaseClients(t)
	for i, contexts := range [][]wire.CreateContext{
		{{Name: "RqLs", Data: []byte{1}}},
		{{Name: "RqLs", Data: make([]byte, 52)}, {Name: "RqLs", Data: make([]byte, 52)}},
	} {
		request := leaseCreateRequest(fmt.Sprintf("invalid-%d", i))
		request.OplockLevel, request.Contexts = leaseOplockLevel, contexts
		message, receiveErr := first.receive(first.sendRawCreate(t, request))
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if message.Header.Status != smb.StatusInvalidParameter {
			t.Fatalf("malformed lease = %#x", message.Header.Status)
		}
		resolved, err := server.options.Storage.Lookup(first.ctx, request.Name)
		if err != nil || resolved.Exists {
			t.Fatalf("malformed lease created file: %+v, error %v", resolved, err)
		}
	}
}

func TestCreateLeaseKeyCannotMoveToAnotherFile(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	first.create(t, leaseCreateRequest("first"), leaseV2(1, 3))
	second.create(t, leaseCreateRequest("second"), nil)
	id := first.send(t, leaseCreateRequest("second"), leaseV2(1, 3))
	message, err := first.receive(id)
	if err != nil || message.Header.Status != smb.StatusInvalidParameter {
		t.Fatalf("lease key reuse = %#x, error %v", message.Header.Status, err)
	}
}

func TestCreateLeaseResponseWithoutParentFlagClearsParent(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	lease := leaseV2(1, 3)
	lease.Flags = 0
	result := first.create(t, leaseCreateRequest("file"), lease)
	if result.Lease == nil || result.Lease.ParentKey != [16]byte{} || result.Lease.Flags != 0 {
		t.Fatalf("unmarked parent = %+v", result.Lease)
	}
	second.create(t, leaseCreateRequest("file"), nil)
}

func TestCreateLeaseResponseCarriesPendingSharedBreak(t *testing.T) {
	server, first, observer := newCreateLeaseClients(t)
	second := loginCreateLeaseClient(t, server, 2)
	opened := first.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	id := observer.send(t, leaseCreateRequest("file"), nil)
	notification, err := first.client.WaitLeaseBreak(first.ctx)
	if err != nil {
		t.Fatal(err)
	}
	joined := second.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 7 || joined.Lease.Epoch != notification.Epoch || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress {
		t.Fatalf("pending response = %+v", joined.Lease)
	}
	open, status := server.options.State.Find(state.FileID(opened.Reply.ID), state.Binding{SessionID: first.session.SessionID, TreeID: first.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("shared open = %#x", status)
	}
	current, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || !current.Breaking || current.State != 7 || current.BreakTo != 3 {
		t.Fatalf("pending state = %+v", current)
	}
	first.ack(t, notification)
	observer.created(t, id)
}
