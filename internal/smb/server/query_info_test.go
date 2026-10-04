package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestQueryInfoClassesMatchLiveAttributes(t *testing.T) {
	f := newQueryInfoFixture(t)
	for _, kind := range []smb.Kind{smb.KindFile, smb.KindDirectory} {
		path := fmt.Sprintf("object-%d", kind)
		_, open := f.create(t, path, kind)
		if kind == smb.KindFile {
			data := []byte("buffered length")
			if n, err := f.storage.WriteAt(f.ctx, open.Handle, data, 4096); err != nil || n != len(data) {
				t.Fatalf("WriteAt = %d, %v", n, err)
			}
		}
		attr, err := f.storage.GetAttr(f.ctx, open.Object)
		if err != nil {
			t.Fatal(err)
		}
		if kind == smb.KindFile && attr.Size != 4096+uint64(len("buffered length")) {
			t.Fatalf("live size = %d", attr.Size)
		}
		for _, class := range queryFileClasses {
			t.Run(fmt.Sprintf("kind_%d/class_%d", kind, class), func(t *testing.T) {
				data := queryData(t, f.query(t, open, wire.InfoFile, uint8(class), 4096), smb.StatusSuccess)
				checkQueryClass(t, f, open, attr, "\\"+path, class, data)
			})
		}
	}
}

func TestQueryInfoNamedStreamOpen(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, base := f.create(t, "stream-file", smb.KindFile)
	_, stream := f.create(t, "stream-file:fork", smb.KindFile)
	for _, write := range []struct {
		data   string
		open   state.Open
		offset uint64
	}{
		{"base-file", base, 4096},
		{"fork", stream, 0},
	} {
		if n, err := f.storage.WriteAt(f.ctx, write.open.Handle, []byte(write.data), write.offset); err != nil || n != len(write.data) {
			t.Fatalf("WriteAt = %d, %v", n, err)
		}
	}
	baseAttr, err := f.storage.GetAttr(f.ctx, base.Object)
	if err != nil {
		t.Fatal(err)
	}
	streamAttr, err := f.storage.GetAttr(f.ctx, stream.Object)
	if err != nil {
		t.Fatal(err)
	}
	wantStreams := wire.FileStreamInformation{Entries: []wire.FileStreamEntry{
		{Name: "::$DATA", Size: 4105, AllocationSize: baseAttr.AllocationSize},
		{Name: ":fork:$DATA", Size: 4, AllocationSize: streamAttr.AllocationSize},
	}}
	for _, selected := range []struct {
		name string
		open state.Open
		size uint64
	}{
		{"\\stream-file", base, 4105},
		{"\\stream-file:fork:$DATA", stream, 4},
	} {
		t.Run(selected.name, func(t *testing.T) {
			data := queryData(t, f.query(t, selected.open, wire.InfoFile, uint8(wire.ClassFileStream), 4096), smb.StatusSuccess)
			decodeQueryClass(t, data, wire.DecodeFileStreamInformation, wantStreams)
			data = queryData(t, f.query(t, selected.open, wire.InfoFile, uint8(wire.ClassFileName), 4096), smb.StatusSuccess)
			decodeQueryClass(t, data, wire.DecodeFileNameInformation, wire.FileNameInformation{Name: selected.name})
			data = queryData(t, f.query(t, selected.open, wire.InfoFile, uint8(wire.ClassFileStandard), 24), smb.StatusSuccess)
			info, err := wire.DecodeFileStandardInformation(data)
			if err != nil {
				t.Fatal(err)
			}
			if info.EndOfFile != selected.size {
				t.Fatalf("EndOfFile = %d, want %d", info.EndOfFile, selected.size)
			}
			f.echo(t)
		})
	}
}

func TestQueryInfoShortOutputBuffers(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, open := f.create(t, "long-file-name", smb.KindFile)
	for _, class := range queryFileClasses {
		if class == wire.ClassFileStream {
			continue // Stream entries have separate boundary tests below.
		}
		t.Run(fmt.Sprintf("class_%d", class), func(t *testing.T) {
			full := queryData(t, f.query(t, open, wire.InfoFile, uint8(class), 4096), smb.StatusSuccess)
			length := len(full)
			if length < 0 || length > 4096 {
				t.Fatal("output exceeds requested length")
				return
			}
			fullLength := uint32(length)
			fixed := fileInfoFixedSize(class)
			for _, length := range []uint32{0, fixed - 1} {
				message := f.query(t, open, wire.InfoFile, uint8(class), length)
				if message.Header.Status != smb.StatusInfoLengthMismatch {
					t.Fatalf("length %d: status = %#x", length, message.Header.Status)
				}
				if _, err := wire.DecodeErrorResponse(message); err != nil {
					t.Fatal(err)
				}
			}
			for _, length := range []uint32{fixed, fullLength} {
				checkQueryPrefix(t, f, open, class, length, full)
			}
			if fullLength > fixed {
				length := fullLength - 1
				got := queryData(t, f.query(t, open, wire.InfoFile, uint8(class), length), smb.StatusBufferOverflow)
				if !bytes.Equal(got, full[:length]) {
					t.Fatal("variable prefix changed")
				}
			}
		})
	}
}

func TestQueryInfoStreamBufferBoundaries(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, open := f.create(t, "stream-buffers", smb.KindFile)
	f.create(t, "stream-buffers:fork", smb.KindFile)
	// The entries occupy 38 and 46 bytes; the first aligns to 40 when linked.
	entries := []wire.FileStreamEntry{{Name: "::$DATA"}, {Name: ":fork:$DATA"}}
	for _, test := range []struct {
		count  int
		length uint32
		status smb.Status
	}{
		{0, 0, smb.StatusInfoLengthMismatch},
		{0, 23, smb.StatusInfoLengthMismatch},
		{0, 24, smb.StatusBufferOverflow},
		{0, 37, smb.StatusBufferOverflow},
		{1, 38, smb.StatusBufferOverflow},
		{1, 39, smb.StatusBufferOverflow},
		{1, 40, smb.StatusBufferOverflow},
		{1, 85, smb.StatusBufferOverflow},
		{2, 86, smb.StatusSuccess},
		{2, 87, smb.StatusSuccess},
	} {
		t.Run(fmt.Sprintf("length_%d", test.length), func(t *testing.T) {
			message := f.query(t, open, wire.InfoFile, uint8(wire.ClassFileStream), test.length)
			if message.Header.Status != test.status {
				t.Fatalf("status = %#x, want %#x", message.Header.Status, test.status)
			}
			if test.status == smb.StatusInfoLengthMismatch {
				if _, err := wire.DecodeErrorResponse(message); err != nil {
					t.Fatal(err)
				}
			} else {
				data := queryData(t, message, test.status)
				want := wire.FileStreamInformation{}
				if test.count > 0 {
					want.Entries = entries[:test.count]
				}
				decodeQueryClass(t, data, wire.DecodeFileStreamInformation, want)
				if uint64(len(data)) > uint64(test.length) {
					t.Fatalf("output length = %d, buffer = %d", len(data), test.length)
				}
				if test.count > 0 {
					last := []int{0, 40}[test.count-1]
					if next := binary.LittleEndian.Uint32(data[last : last+4]); next != 0 {
						t.Fatalf("final NextEntryOffset = %d", next)
					}
				}
			}
			f.echo(t)
		})
	}
}

func TestUnsupportedQueryInfoAndObjectIDKeepConnection(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, open := f.create(t, "unsupported", smb.KindFile)
	for class := 0; class <= 255; class++ {
		if fileInfoFixedSize(wire.FileInfoClass(class)) != 0 {
			continue
		}
		message := f.query(t, open, wire.InfoFile, uint8(class), 4096)
		want := smb.StatusInvalidInfoClass
		if class == 48 { // FileNormalizedNameInformation.
			want = smb.StatusNotSupported
		}
		if message.Header.Status != want {
			t.Fatalf("unsupported file class %d: status = %#x, want %#x", class, message.Header.Status, want)
		}
		if _, err := wire.DecodeErrorResponse(message); err != nil {
			t.Fatal(err)
		}
		f.echo(t)
	}
	for _, test := range []struct {
		infoType wire.InfoType
		class    uint8
		status   smb.Status
	}{
		{wire.InfoFilesystem, 8, smb.StatusNotSupported},       // FileFsObjectIdInformation (#87).
		{wire.InfoFilesystem, 100, smb.StatusInvalidInfoClass}, // FileFsPosixInformation.
		{wire.InfoSecurity, 0, smb.StatusNotSupported},
		{4, 0, smb.StatusNotSupported}, // Quota.
		{0, 0, smb.StatusInvalidParameter},
		{5, 0, smb.StatusInvalidParameter},
		{255, 0, smb.StatusInvalidParameter},
	} {
		message := f.query(t, open, test.infoType, test.class, 4096)
		if message.Header.Status != test.status {
			t.Fatalf("unsupported type %d class %d: status = %#x, want %#x", test.infoType, test.class, message.Header.Status, test.status)
		}
		if _, err := wire.DecodeErrorResponse(message); err != nil {
			t.Fatal(err)
		}
		f.echo(t)
	}
}

func TestQueryInfoFileIDIndependentOfOpenID(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, first := f.create(t, "identity", smb.KindFile)
	second := f.open(t, first.Object)
	if first.ID == second.ID {
		t.Fatal("two opens have the same ID")
	}
	one := queryData(t, f.query(t, first, wire.InfoFile, uint8(wire.ClassFileID), 24), smb.StatusSuccess)
	two := queryData(t, f.query(t, second, wire.InfoFile, uint8(wire.ClassFileID), 24), smb.StatusSuccess)
	if !bytes.Equal(one, two) {
		t.Fatal("FileIdInformation depends on the open ID")
	}
	attr, err := f.storage.GetAttr(f.ctx, first.Object)
	if err != nil {
		t.Fatal(err)
	}
	checkQueryClass(t, f, first, attr, "\\identity", wire.ClassFileID, one)
}

func TestQueryInfoDeletePendingAndCurrentName(t *testing.T) {
	f := newQueryInfoFixture(t)
	resolved, open := f.create(t, "before", smb.KindFile)
	missing, err := f.storage.Lookup(f.ctx, "after")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.storage.Rename(f.ctx, smb.RenameRequest{Source: resolved.Name, Destination: missing.Name, SourceInode: open.Object.Inode}); err != nil {
		t.Fatal(err)
	}
	data := queryData(t, f.query(t, open, wire.InfoFile, uint8(wire.ClassFileName), 4096), smb.StatusSuccess)
	decodeQueryClass(t, data, wire.DecodeFileNameInformation, wire.FileNameInformation{Name: "\\after"})
	if status := f.server.options.State.SetDelete(open.ID, open.Binding, missing.Name, true); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	for _, class := range []wire.FileInfoClass{wire.ClassFileStandard, wire.ClassFileAll} {
		data := queryData(t, f.query(t, open, wire.InfoFile, uint8(class), 4096), smb.StatusSuccess)
		if class == wire.ClassFileStandard {
			info, err := wire.DecodeFileStandardInformation(data)
			if err != nil || !info.DeletePending {
				t.Fatalf("Standard deletion = %+v, %v", info, err)
			}
		} else {
			info, err := wire.DecodeFileAllInformation(data)
			if err != nil || !info.Standard.DeletePending || info.Name.Name != "\\after" {
				t.Fatalf("All deletion/name = %+v, %v", info, err)
			}
		}
	}
}

func TestQueryInfoInvalidOpenAndStorageErrorKeepConnection(t *testing.T) {
	f := newQueryInfoFixture(t)
	resolved, open := f.create(t, "removed", smb.KindFile)
	invalid := open
	invalid.ID.Volatile++
	message := f.query(t, invalid, wire.InfoFile, uint8(wire.ClassFileBasic), 40)
	if message.Header.Status != smb.StatusFileClosed {
		t.Fatalf("invalid open: %#x", message.Header.Status)
	}
	f.echo(t)
	if err := f.storage.Remove(f.ctx, resolved.Name, open.Object.Inode); err != nil {
		t.Fatal(err)
	}
	message = f.query(t, open, wire.InfoFile, uint8(wire.ClassFileName), 4096)
	if message.Header.Status != smb.StatusObjectNameNotFound {
		t.Fatalf("unlinked name: %#x", message.Header.Status)
	}
	f.echo(t)
}
