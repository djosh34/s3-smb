package server

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type setInfoFixture struct {
	storage smb.Storage
	client  *smbtest.Client
	ctx     context.Context
	open    state.Open
	session smbtest.Session
	nextID  uint64
}

func newSetInfoFixture(t *testing.T, access uint32) *setInfoFixture {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := newFilesMetaClient(t, server)
	open := insertFilesMetaOpen(t, server, session, "data", access)
	return &setInfoFixture{storage: options.Storage, client: client, ctx: ctx, session: session, open: open, nextID: session.NextMessageID}
}

func (f *setInfoFixture) set(t *testing.T, info wire.SetInfoRequest, want smb.Status) {
	t.Helper()
	body, err := wire.EncodeSetInfoRequest(info)
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(f.ctx, t, f.client, wire.Message{Header: wire.Header{
		Command: wire.SetInfo, MessageID: f.nextID, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: 1, Credit: 16,
	}, Body: body})[0]
	f.nextID++
	if response.Header.Status != want {
		t.Fatalf("SET_INFO type %d class %d: status %#x, want %#x", info.InfoType, info.InfoClass, response.Header.Status, want)
	}
	if want == smb.StatusSuccess {
		if _, decodeErr := wire.DecodeSetInfoResponse(response); decodeErr != nil {
			t.Fatal(decodeErr)
		}
	}
}

func (f *setInfoFixture) basic(t *testing.T, info wire.FileBasicInformation, want smb.Status) {
	t.Helper()
	buffer, err := wire.EncodeFileBasicInformation(info)
	if err != nil {
		t.Fatal(err)
	}
	f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileBasic), Input: buffer}, want)
}

func (f *setInfoFixture) size(t *testing.T, class wire.FileInfoClass, size uint64, want smb.Status) {
	t.Helper()
	var buffer []byte
	var err error
	if class == wire.ClassFileEndOfFile {
		buffer, err = wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: size})
	} else {
		buffer, err = wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: size})
	}
	if err != nil {
		t.Fatal(err)
	}
	f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(class), Input: buffer}, want)
}

func (f *setInfoFixture) attr(t *testing.T) smb.Attr {
	t.Helper()
	attr, err := f.storage.GetAttr(f.ctx, f.open.Object)
	if err != nil {
		t.Fatal(err)
	}
	return attr
}

func (f *setInfoFixture) write(t *testing.T, data string) {
	t.Helper()
	count, err := f.storage.WriteAt(f.ctx, f.open.Handle, []byte(data), 0)
	if err != nil || count != len(data) {
		t.Fatalf("WriteAt = %d, %v", count, err)
	}
}

func filetime(t *testing.T, value time.Time) wire.Filetime {
	t.Helper()
	encoded, err := wire.EncodeFiletime(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSetInfoTimestampSentinelsWithoutOverflowIssue111(t *testing.T) {
	for _, sentinel := range []wire.Filetime{wire.FiletimeUnchanged, wire.FiletimeSuppress, wire.FiletimeResume} {
		t.Run(fmt.Sprintf("%016x", sentinel), func(t *testing.T) {
			f := newSetInfoFixture(t, 0x100)
			created := time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC)
			accessed := created.Add(time.Hour)
			modified := created.Add(2 * time.Hour)
			changed := created.Add(3 * time.Hour)
			attributes := uint32(0x20)
			if err := f.storage.SetAttr(f.ctx, f.open.Object, smb.AttrChange{Created: &created, Accessed: &accessed, Modified: &modified, Changed: &changed, Attributes: &attributes}); err != nil {
				t.Fatal(err)
			}
			before := f.attr(t)
			f.basic(t, wire.FileBasicInformation{Created: sentinel, Accessed: sentinel, Modified: sentinel, Changed: sentinel}, smb.StatusSuccess)
			after := f.attr(t)
			if !after.Created.Equal(before.Created) || !after.Accessed.Equal(before.Accessed) || !after.Modified.Equal(before.Modified) || !after.Changed.Equal(before.Changed) || after.Attributes != before.Attributes {
				t.Fatalf("sentinel changed stored metadata: before %+v, after %+v", before, after)
			}
			// The sentinels affect only SET_INFO, never later automatic I/O updates.
			f.write(t, "later I/O")
			after = f.attr(t)
			if !after.Modified.After(before.Modified) || !after.Changed.After(before.Changed) {
				t.Fatalf("sentinel suppressed write timestamps: before %+v, after %+v", before, after)
			}
		})
	}
}

func TestSetInfoUnixEpochIsAnExplicitTime(t *testing.T) {
	f := newSetInfoFixture(t, 0x100)
	epoch := time.Unix(0, 0).UTC()
	value := filetime(t, epoch)
	f.basic(t, wire.FileBasicInformation{Created: value, Accessed: value, Modified: value, Changed: value, Attributes: 0x20}, smb.StatusSuccess)
	attr := f.attr(t)
	if !attr.Created.Equal(epoch) || !attr.Accessed.Equal(epoch) || !attr.Modified.Equal(epoch) || !attr.Changed.Equal(epoch) || attr.Attributes != 0x20 {
		t.Fatalf("epoch or attributes not stored: %+v", attr)
	}
}

func TestSetInfoBasicFieldsAreIndependent(t *testing.T) {
	f := newSetInfoFixture(t, 0x100)
	before := f.attr(t)
	created := time.Date(1985, 3, 4, 5, 6, 7, 800, time.UTC)
	accessed := created.Add(time.Hour)
	modified := created.Add(2 * time.Hour)
	changed := created.Add(3 * time.Hour)
	f.basic(t, wire.FileBasicInformation{Created: filetime(t, created), Accessed: filetime(t, accessed), Modified: filetime(t, modified), Changed: filetime(t, changed)}, smb.StatusSuccess)
	after := f.attr(t)
	if !after.Created.Equal(created) || !after.Accessed.Equal(accessed) || !after.Modified.Equal(modified) || !after.Changed.Equal(changed) || after.Attributes != before.Attributes {
		t.Fatalf("basic fields not stored independently: %+v", after)
	}
	// Omitted times still stay unchanged when another field is present.
	created = created.Add(time.Minute)
	f.basic(t, wire.FileBasicInformation{Created: filetime(t, created), Accessed: wire.FiletimeSuppress, Modified: wire.FiletimeResume, Attributes: 0x20}, smb.StatusSuccess)
	after = f.attr(t)
	if !after.Created.Equal(created) || !after.Accessed.Equal(accessed) || !after.Modified.Equal(modified) || !after.Changed.Equal(changed) || after.Attributes != 0x20 {
		t.Fatalf("mixed sentinel update changed other fields: %+v", after)
	}
}

func TestSetInfoAllocationBelowEOFShrinksIssue108(t *testing.T) {
	for _, allocation := range []uint64{0, 3, 7, 8192} {
		t.Run(fmt.Sprint(allocation), func(t *testing.T) {
			f := newSetInfoFixture(t, 2)
			f.write(t, "1234567")
			before := f.attr(t)
			f.size(t, wire.ClassFileAllocation, allocation, smb.StatusSuccess)
			want := min(allocation, uint64(7))
			after := f.attr(t)
			if after.Size != want {
				t.Fatalf("allocation %d: EOF = %d, want %d", allocation, after.Size, want)
			}
			if allocation >= 7 && (!after.Modified.Equal(before.Modified) || !after.Changed.Equal(before.Changed) || after.AllocationSize != before.AllocationSize) {
				t.Fatalf("allocation growth hint changed metadata: before %+v, after %+v", before, after)
			}
			if err := f.storage.Flush(f.ctx, f.open.Handle, smb.SyncData); err != nil {
				t.Fatal(err)
			}
			if after = f.attr(t); after.Size != want {
				t.Fatalf("flush restored old EOF: %+v", after)
			}
		})
	}
}

func TestSetInfoEndOfFileTruncatesAndExtends(t *testing.T) {
	f := newSetInfoFixture(t, 2)
	f.write(t, "1234567")
	f.size(t, wire.ClassFileEndOfFile, 3, smb.StatusSuccess)
	f.size(t, wire.ClassFileEndOfFile, 9, smb.StatusSuccess)
	if err := f.storage.Flush(f.ctx, f.open.Handle, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 9)
	count, err := f.storage.ReadAt(f.ctx, f.open.Handle, data, 0)
	if err != nil || count != len(data) || !bytes.Equal(data, []byte{'1', '2', '3', 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("truncate/extend read = %q, %d, %v", data, count, err)
	}
	f.size(t, wire.ClassFileEndOfFile, 0, smb.StatusSuccess)
	if attr := f.attr(t); attr.Size != 0 {
		t.Fatalf("EOF = %d", attr.Size)
	}
}

func TestSetInfoChecksGrantedAccessPerClass(t *testing.T) {
	for _, access := range []uint32{0, 4, 2, 0x100} {
		t.Run(fmt.Sprintf("%x", access), func(t *testing.T) {
			f := newSetInfoFixture(t, access)
			f.write(t, "1234567")
			want := smb.StatusAccessDenied
			if access&0x100 != 0 {
				want = smb.StatusSuccess
			}
			f.basic(t, wire.FileBasicInformation{Attributes: 0x20}, want)
			if access&0x100 == 0 && f.attr(t).Attributes == 0x20 {
				t.Fatal("denied basic update changed attributes")
			}
			want = smb.StatusAccessDenied
			if access&2 != 0 {
				want = smb.StatusSuccess
			}
			f.size(t, wire.ClassFileEndOfFile, 3, want)
			f.size(t, wire.ClassFileAllocation, 0, want)
			if access&2 == 0 && f.attr(t).Size != 7 {
				t.Fatal("denied size update changed EOF")
			}
		})
	}
}

func TestSetInfoUnsupportedClassesAndTypesKeepConnection(t *testing.T) {
	f := newSetInfoFixture(t, 0x102)
	before := f.attr(t)
	for _, class := range []uint8{11, uint8(wire.ClassFileStandard), uint8(wire.ClassFileRename), uint8(wire.ClassFileDisposition), 255} {
		f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: class, Input: []byte{1}}, smb.StatusNotSupported)
	}
	for _, infoType := range []wire.InfoType{0, wire.InfoFilesystem, wire.InfoSecurity, 4, 255} {
		f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: infoType, InfoClass: uint8(wire.ClassFileBasic)}, smb.StatusNotSupported)
	}
	if after := f.attr(t); after != before {
		t.Fatalf("unsupported request changed metadata: before %+v, after %+v", before, after)
	}
	if response := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, f.nextID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("unsupported SET_INFO dropped connection")
	}
}

func TestSetInfoInvalidBuffersAndFileIDsKeepConnection(t *testing.T) {
	f := newSetInfoFixture(t, 0x102)
	before := f.attr(t)
	for _, class := range []wire.FileInfoClass{wire.ClassFileBasic, wire.ClassFileEndOfFile, wire.ClassFileAllocation} {
		f.set(t, wire.SetInfoRequest{ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(class), Input: []byte{1}}, smb.StatusInfoLengthMismatch)
	}
	for _, class := range []wire.FileInfoClass{wire.ClassFileEndOfFile, wire.ClassFileAllocation} {
		f.size(t, class, ^uint64(0), smb.StatusInvalidParameter)
	}
	f.set(t, wire.SetInfoRequest{ID: wire.FileID{Persistent: f.open.ID.Persistent, Volatile: f.open.ID.Volatile + 1}, InfoType: wire.InfoFile}, smb.StatusFileClosed)
	f.set(t, wire.SetInfoRequest{ID: wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}, InfoType: wire.InfoFile}, smb.StatusFileClosed)
	if after := f.attr(t); after != before {
		t.Fatalf("invalid request changed metadata: before %+v, after %+v", before, after)
	}
	if response := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, f.nextID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("invalid SET_INFO dropped connection")
	}
}

func TestSetInfoStorageErrorsReturnStatus(t *testing.T) {
	f := newSetInfoFixture(t, 2)
	f.size(t, wire.ClassFileEndOfFile, 1<<62, smb.StatusFileTooLarge)
	if f.attr(t).Size != 0 {
		t.Fatal("failed size update changed EOF")
	}
	if response := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, f.nextID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("storage error dropped connection")
	}
}
