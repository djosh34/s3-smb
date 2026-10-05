package server

import (
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	exclusiveNow = lockExclusive | lockFailImmediately
	sharedNow    = lockShared | lockFailImmediately
)

// lockRange sends one LOCK of offset and length with flags and fails the
// test unless the status is want.
func lockRange(t *testing.T, client *testClient, id wire.FileID, offset, length uint64, flags uint32, want smb.Status) {
	t.Helper()
	if status := client.lock(t, wire.LockRequest{ID: id, Elements: []wire.LockElement{{Offset: offset, Length: length, Flags: flags}}}); status != want {
		t.Fatalf("LOCK %d+%d flags %#x: status %#x, want %#x", offset, length, flags, status, want)
	}
}

// expectIO checks the READ and WRITE statuses of 8 bytes at offset.
func expectIO(t *testing.T, client *testClient, id wire.FileID, offset uint64, read, write smb.Status) {
	t.Helper()
	if _, status := client.read(t, wire.ReadRequest{ID: id, Offset: offset, Length: 8}); status != read {
		t.Errorf("READ at %d: status %#x, want %#x", offset, status, read)
	}
	if status := client.write(t, wire.WriteRequest{ID: id, Offset: offset, Data: []byte("12345678")}); status != write {
		t.Errorf("WRITE at %d: status %#x, want %#x", offset, status, write)
	}
}

func TestLockRanges(t *testing.T) {
	client := newTestServer(t).connect(t)
	owner, other := client.open(t, "file"), client.open(t, "file")
	writeFile(t, client, owner, make([]byte, 64))

	// An exclusive range allows only its owner's I/O.
	lockRange(t, client, owner, 0, 8, lockExclusive, smb.StatusSuccess)
	expectIO(t, client, owner, 0, smb.StatusSuccess, smb.StatusSuccess)
	expectIO(t, client, other, 0, smb.StatusFileLockConflict, smb.StatusFileLockConflict)

	// A lock that asks to wait fails at once with one synchronous reply.
	body := encode(t, wire.EncodeLockRequest, wire.LockRequest{ID: other, Elements: []wire.LockElement{{Offset: 4, Length: 8, Flags: lockShared}}})
	if reply := client.next(t, client.send(t, wire.Lock, body, 1)).Header; reply.Status != smb.StatusLockNotGranted || reply.Flags&wire.FlagAsync != 0 {
		t.Fatalf("waiting LOCK reply %+v", reply)
	}

	// A vector that fails on its second range keeps none of it.
	conflict := wire.LockRequest{ID: other, Elements: []wire.LockElement{{Offset: 16, Length: 8, Flags: exclusiveNow}, {Length: 8, Flags: exclusiveNow}}}
	if status := client.lock(t, conflict); status != smb.StatusLockNotGranted {
		t.Fatalf("conflicting vector: status %#x", status)
	}
	expectIO(t, client, owner, 16, smb.StatusSuccess, smb.StatusSuccess)

	// Unlock needs the owner and the exact range.
	lockRange(t, client, other, 0, 8, lockUnlock, smb.StatusRangeNotLocked)
	lockRange(t, client, owner, 0, 7, lockUnlock, smb.StatusRangeNotLocked)
	lockRange(t, client, owner, 0, 8, lockUnlock, smb.StatusSuccess)
	expectIO(t, client, other, 0, smb.StatusSuccess, smb.StatusSuccess)

	// Shared ranges allow everyone's reads and nobody's writes.
	lockRange(t, client, owner, 32, 8, sharedNow, smb.StatusSuccess)
	lockRange(t, client, other, 32, 8, lockShared, smb.StatusSuccess)
	lockRange(t, client, other, 32, 8, exclusiveNow, smb.StatusLockNotGranted)
	expectIO(t, client, owner, 32, smb.StatusSuccess, smb.StatusFileLockConflict)
	expectIO(t, client, other, 32, smb.StatusSuccess, smb.StatusFileLockConflict)

	// Closing an open releases its ranges.
	lockRange(t, client, owner, 48, 8, lockExclusive, smb.StatusSuccess)
	closeOK(t, client, owner)
	lockRange(t, client, other, 48, 8, exclusiveNow, smb.StatusSuccess)
	lockRange(t, client, owner, 48, 8, exclusiveNow, smb.StatusFileClosed)
}

func TestLockStreamsAndBaseAreSeparate(t *testing.T) {
	client := newTestServer(t).connect(t)
	base, stream := client.open(t, "file"), client.open(t, "file:fork")
	baseReader, streamReader := client.open(t, "file"), client.open(t, "file:fork")
	lockRange(t, client, base, 0, 8, lockExclusive, smb.StatusSuccess)
	expectIO(t, client, streamReader, 0, smb.StatusEndOfFile, smb.StatusSuccess)
	lockRange(t, client, stream, 0, 8, lockExclusive, smb.StatusSuccess)
	expectIO(t, client, baseReader, 0, smb.StatusFileLockConflict, smb.StatusFileLockConflict)
	expectIO(t, client, streamReader, 0, smb.StatusFileLockConflict, smb.StatusFileLockConflict)
}

// A refused LOCK takes no range: afterwards another open can lock the whole file.
func TestLockRefusals(t *testing.T) {
	client := newTestServer(t).connect(t)
	id, other := client.open(t, "file"), client.open(t, "file")
	directory := openDirectory(t, client, "directory")
	stale := id
	stale.Volatile++
	free := wire.LockElement{Offset: 100, Length: 8, Flags: exclusiveNow}
	for _, test := range []struct {
		name     string
		elements []wire.LockElement
		id       wire.FileID
		want     smb.Status
	}{
		{"no flags", []wire.LockElement{free, {Length: 8}}, id, smb.StatusInvalidParameter},
		{"shared and exclusive", []wire.LockElement{free, {Length: 8, Flags: lockShared | lockExclusive | lockFailImmediately}}, id, smb.StatusInvalidParameter},
		{"unlock and lock", []wire.LockElement{free, {Length: 8, Flags: lockUnlock | lockShared}}, id, smb.StatusInvalidParameter},
		{"unlock with fail immediately", []wire.LockElement{{Length: 8, Flags: lockUnlock | lockFailImmediately}}, id, smb.StatusInvalidParameter},
		{"unknown flag", []wire.LockElement{free, {Length: 8, Flags: 0x20 | exclusiveNow}}, id, smb.StatusInvalidParameter},
		{"lock after unlock", []wire.LockElement{{Length: 8, Flags: lockUnlock}, free}, id, smb.StatusInvalidParameter},
		{"waiting first range of many", []wire.LockElement{{Length: 8, Flags: lockExclusive}, free}, id, smb.StatusInvalidParameter},
		{"waiting second range of many", []wire.LockElement{free, {Length: 8, Flags: lockShared}}, id, smb.StatusInvalidParameter},
		{"past the last byte", []wire.LockElement{free, {Offset: math.MaxUint64, Length: 2, Flags: exclusiveNow}}, id, smb.StatusInvalidLockRange},
		{"directory", []wire.LockElement{free}, directory, smb.StatusInvalidParameter},
		{"closed open", []wire.LockElement{free}, stale, smb.StatusFileClosed},
	} {
		if status := client.lock(t, wire.LockRequest{ID: test.id, Elements: test.elements}); status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	malformed := encode(t, wire.EncodeLockRequest, wire.LockRequest{ID: id, Elements: []wire.LockElement{free}})
	if status := client.call(t, wire.Lock, malformed[:24], 1).Header.Status; status != smb.StatusInvalidParameter {
		t.Errorf("malformed LOCK: status %#x", status)
	}
	lockRange(t, client, other, 0, math.MaxUint64, exclusiveNow, smb.StatusSuccess)
}
