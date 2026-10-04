package server

import (
	"fmt"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReadDirectoryReturnsInvalidDeviceRequest(t *testing.T) {
	client := newReadWriteClient(t, smbtest.NewStorage(t))
	request := createRequest("directory", fileCreateDisposition)
	request.Options = fileDirectoryFile
	opened := createdFile(t, client.create(t, request))
	for _, read := range []wire.ReadRequest{
		{ID: opened.ID, Length: 10, MinimumCount: 1},
		{ID: opened.ID, Length: 10, MinimumCount: 11},
		{ID: opened.ID},
		{ID: opened.ID, MinimumCount: 2592},
	} {
		requireIOStatus(t, client.read(t, read, 1), smb.StatusInvalidDeviceRequest)
	}
	requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
}

func TestCreateExecuteAccessAllowsRead(t *testing.T) {
	client := newReadWriteClient(t, smbtest.NewStorage(t))
	seed := createdFile(t, client.create(t, createRequest("executable", fileCreateDisposition)))
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: seed.ID, Data: []byte("executable data")}, 1), smb.StatusSuccess)
	requireIOStatus(t, client.close(t, seed.ID, 0), smb.StatusSuccess)
	for _, access := range []uint32{0x20, genericExecute, 0x80} {
		t.Run(fmt.Sprintf("access_%x", access), func(t *testing.T) {
			request := createRequest("executable", fileOpen)
			request.DesiredAccess = access
			opened := createdFile(t, client.create(t, request))
			response := client.read(t, wire.ReadRequest{ID: opened.ID, Length: 10}, 1)
			if access == 0x80 {
				requireIOStatus(t, response, smb.StatusAccessDenied)
			} else {
				requireIOStatus(t, response, smb.StatusSuccess)
				read, err := wire.DecodeReadResponse(response)
				if err != nil || string(read.Data) != "executable" {
					t.Fatalf("READ: %+v, %v", read, err)
				}
				open, status := client.server.options.State.Find(state.FileID(opened.ID), state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID})
				if status != smb.StatusSuccess || open.GrantedAccess&0x21 != 0x21 {
					t.Fatalf("execute grant: %#x, status %#x", open.GrantedAccess, status)
				}
			}
			requireIOStatus(t, client.write(t, wire.WriteRequest{ID: opened.ID, Data: []byte("denied")}, 1), smb.StatusAccessDenied)
			requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
		})
	}
}

func TestEmptyReadIgnoresExclusiveRangeAndEOF(t *testing.T) {
	client := newReadWriteClient(t, smbtest.NewStorage(t))
	owner := createdFile(t, client.create(t, createRequest("locked", fileCreateDisposition)))
	other := createdFile(t, client.create(t, createRequest("locked", fileOpen)))
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: owner.ID, Data: []byte("data")}, 1), smb.StatusSuccess)
	binding := state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID}
	if status := client.server.options.State.Lock(state.FileID(owner.ID), binding, []state.Range{{Offset: 0, Length: 64, Exclusive: true}}, false); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: other.ID, Offset: 1, Length: 1}, 1), smb.StatusFileLockConflict)
	for _, offset := range []uint64{1, 5, 64, math.MaxInt64} {
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			response := client.read(t, wire.ReadRequest{ID: other.ID, Offset: offset}, 1)
			requireIOStatus(t, response, smb.StatusSuccess)
			read, err := wire.DecodeReadResponse(response)
			if err != nil || len(read.Data) != 0 || read.Remaining != 0 {
				t.Fatalf("empty READ: %+v, %v", read, err)
			}
		})
	}
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: other.ID, Offset: 1, MinimumCount: 1}, 1), smb.StatusEndOfFile)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: other.ID, Channel: 1}, 1), smb.StatusInvalidParameter)
	request := createRequest("locked", fileOpen)
	request.DesiredAccess = 0x80
	metadata := createdFile(t, client.create(t, request))
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: metadata.ID, Offset: 1}, 1), smb.StatusAccessDenied)
	requireIOStatus(t, client.close(t, metadata.ID, 0), smb.StatusSuccess)
	requireIOStatus(t, client.close(t, other.ID, 0), smb.StatusSuccess)
	requireIOStatus(t, client.read(t, wire.ReadRequest{ID: other.ID}, 1), smb.StatusFileClosed)
	requireIOStatus(t, client.close(t, owner.ID, 0), smb.StatusSuccess)
}

func TestReadRejectsSignedRangeOverflow(t *testing.T) {
	client := newReadWriteClient(t, smbtest.NewStorage(t))
	opened := createdFile(t, client.create(t, createRequest("range", fileCreateDisposition)))
	for _, test := range []struct {
		offset uint64
		length uint32
		want   smb.Status
	}{
		{math.MaxInt64 - 1, 1, smb.StatusEndOfFile},
		{math.MaxInt64, 0, smb.StatusSuccess},
		{math.MaxInt64, 1, smb.StatusInvalidParameter},
		{math.MaxInt64 - 1, 2, smb.StatusInvalidParameter},
		{uint64(math.MaxInt64) + 1, 0, smb.StatusInvalidParameter},
		{math.MaxUint64, 0, smb.StatusInvalidParameter},
		{math.MaxUint64, 1, smb.StatusInvalidParameter},
	} {
		t.Run(fmt.Sprintf("offset_%d_length_%d", test.offset, test.length), func(t *testing.T) {
			requireIOStatus(t, client.read(t, wire.ReadRequest{ID: opened.ID, Offset: test.offset, Length: test.length}, 1), test.want)
		})
	}
	requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
}
