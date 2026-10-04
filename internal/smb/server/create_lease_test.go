package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type createLeaseClient struct {
	client  *smbtest.Client
	ctx     context.Context
	replies map[uint64]wire.Message
	session smbtest.Session
	next    uint64
}

func newCreateLeaseClients(t *testing.T) (*Server, *createLeaseClient, *createLeaseClient) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server, loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3)
}

func loginCreateLeaseClient(t *testing.T, server *Server, guid byte) *createLeaseClient {
	t.Helper()
	return loginCreateLeaseClientWithCipher(t, server, guid, smb.CipherAES128GCM)
}

func loginCreateLeaseClientWithCipher(t *testing.T, server *Server, guid byte, cipher uint16) *createLeaseClient {
	t.Helper()
	client, ctx := pipeClient(t, server)
	session, err := client.Login(ctx, smbtest.LoginOptions{
		Share: server.options.ShareName, Account: server.options.Account,
		ClientGUID: [16]byte{guid}, Cipher: cipher, Signing: smb.SigningCMAC,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &createLeaseClient{client: client, ctx: ctx, session: session, next: session.NextMessageID, replies: make(map[uint64]wire.Message)}
}

func (client *createLeaseClient) header(command wire.Command) wire.Header {
	header := wire.Header{Command: command, MessageID: client.next, SessionID: client.session.SessionID, TreeID: client.session.TreeID, CreditCharge: 1, Credit: 16}
	client.next++
	return header
}

func (client *createLeaseClient) send(t *testing.T, request wire.CreateRequest, lease *wire.LeaseContext) uint64 {
	t.Helper()
	header := client.header(wire.Create)
	if err := client.client.SendCreate(client.ctx, header, smbtest.CreateOptions{Request: request, Lease: lease}); err != nil {
		t.Fatal(err)
	}
	return header.MessageID
}

func (client *createLeaseClient) sendRawCreate(t *testing.T, request wire.CreateRequest) uint64 {
	t.Helper()
	header := client.header(wire.Create)
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := client.client.Send(client.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	return header.MessageID
}

func (client *createLeaseClient) receive(id uint64) (wire.Message, error) {
	for {
		if message, exists := client.replies[id]; exists {
			delete(client.replies, id)
			return message, nil
		}
		reply, err := client.client.Receive(client.ctx)
		if err != nil {
			return wire.Message{}, err
		}
		for _, message := range reply.Messages {
			if message.Header.Status != smb.StatusPending {
				client.replies[message.Header.MessageID] = message
			}
		}
	}
}

func (client *createLeaseClient) created(t *testing.T, id uint64) smbtest.CreateResult {
	t.Helper()
	message, err := client.receive(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := smbtest.DecodeCreateReply(message)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (client *createLeaseClient) create(t *testing.T, request wire.CreateRequest, lease *wire.LeaseContext) smbtest.CreateResult {
	t.Helper()
	return client.created(t, client.send(t, request, lease))
}

func (client *createLeaseClient) ack(t *testing.T, notification wire.LeaseBreakNotification) {
	t.Helper()
	header := client.header(wire.OplockBreak)
	header.TreeID = 0
	if err := client.client.SendLeaseBreakAcknowledgment(client.ctx, header, wire.LeaseBreakRequest{Key: notification.Key, State: notification.NewState}); err != nil {
		t.Fatal(err)
	}
	message, err := client.receive(header.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Status != smb.StatusSuccess {
		t.Fatalf("lease ACK = %#x", message.Header.Status)
	}
	ack, err := wire.DecodeLeaseBreakResponse(message)
	if err != nil || ack.Key != notification.Key || ack.State != notification.NewState {
		t.Fatalf("lease ACK = %+v, error %v", ack, err)
	}
}

func (client *createLeaseClient) close(t *testing.T, id wire.FileID) {
	t.Helper()
	header := client.header(wire.Close)
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := client.client.Send(client.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	message, err := client.receive(header.MessageID)
	if err != nil || message.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE = %#x, error %v", message.Header.Status, err)
	}
}

func leaseCreateRequest(name string) wire.CreateRequest {
	return wire.CreateRequest{Name: name, DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpenIf}
}

func leaseV2(key byte, requested uint32) *wire.LeaseContext {
	return &wire.LeaseContext{Version: 2, Key: [16]byte{key}, State: requested, Epoch: 7, ParentKey: [16]byte{8}, Flags: leaseParentKeySet, Duration: 999}
}

func assertLeaseGrant(t *testing.T, result smbtest.CreateResult, want uint32) {
	t.Helper()
	if result.Lease == nil || result.Lease.Version != 2 || result.Lease.State != want || result.Lease.Duration != 0 {
		t.Fatalf("lease grant = %+v, want %#x", result.Lease, want)
	}
	level := uint8(0)
	if want != 0 {
		level = leaseOplockLevel
	}
	if result.Reply.OplockLevel != level {
		t.Fatalf("oplock = %#x, want %#x", result.Reply.OplockLevel, level)
	}
}

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

type createLeaseOutcome struct {
	err     error
	message wire.Message
}

func receiveCreateLater(client *createLeaseClient, id uint64) <-chan createLeaseOutcome {
	done := make(chan createLeaseOutcome, 1)
	go func() {
		message, err := client.receive(id)
		done <- createLeaseOutcome{message: message, err: err}
	}()
	return done
}

func assertCreateWaits(t *testing.T, done <-chan createLeaseOutcome) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("CREATE completed before the break ended: %+v, error %v", result.message.Header, result.err)
	case <-time.After(20 * time.Millisecond):
	}
}

func finishCreate(t *testing.T, done <-chan createLeaseOutcome) smbtest.CreateResult {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		decoded, err := smbtest.DecodeCreateReply(result.message)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
		return smbtest.CreateResult{}
	}
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
