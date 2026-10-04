package server

import (
	"bytes"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFilesystemQueriesOnWire(t *testing.T) {
	for _, capacity := range []uint64{0, (3 << 30) + 1023} {
		f := newQueryInfoFixtureWithStorage(t, newFilesMetaStorageWithCapacity(t, capacity))
		_, first := f.create(t, "volume-first", smb.KindFile)
		_, second := f.create(t, "volume-second", smb.KindFile)
		space, err := f.storage.StatFS(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if capacity != 0 && space.Capacity != capacity {
			t.Fatalf("capacity = %d, want %d", space.Capacity, capacity)
		}
		if capacity == 0 && space.Free != 1<<40 {
			t.Fatalf("default free = %d, want 1 TiB", space.Free)
		}
		for _, class := range []wire.FilesystemInfoClass{
			wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize,
			wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute,
		} {
			data := queryData(t, f.query(t, first, wire.InfoFilesystem, uint8(class), 1024), smb.StatusSuccess)
			other := queryData(t, f.query(t, second, wire.InfoFilesystem, uint8(class), 1024), smb.StatusSuccess)
			if !bytes.Equal(data, other) {
				t.Fatalf("class %d depends on the queried file", class)
			}
			switch class {
			case wire.ClassFilesystemVolume:
				decodeQueryClass(t, data, wire.DecodeFilesystemVolumeInformation, wire.FilesystemVolumeInformation{
					Label: "backup", Serial: uint32(space.VolumeID & 0xffffffff),
				})
			case wire.ClassFilesystemSize, wire.ClassFilesystemFullSize:
				checkFilesystemSpace(t, data, class, space)
			case wire.ClassFilesystemDevice:
				decodeQueryClass(t, data, wire.DecodeFilesystemDeviceInformation, wire.FilesystemDeviceInformation{Type: 7, Characteristics: 0x10})
			case wire.ClassFilesystemAttribute:
				decodeQueryClass(t, data, wire.DecodeFilesystemAttributeInformation, wire.FilesystemAttributeInformation{
					Name: "s3-smb", Attributes: smb.AdvertisedFilesystemAttributes, MaxComponentLength: 255,
				})
			}
		}
		f.echo(t)
	}
}

func TestFilesystemQueryOutputLengthsOnWire(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, open := f.create(t, "fs-buffers", smb.KindFile)
	for _, test := range []struct {
		class   wire.FilesystemInfoClass
		minimum uint32
		full    uint32
	}{
		{wire.ClassFilesystemVolume, 18, 30},
		{wire.ClassFilesystemSize, 24, 24},
		{wire.ClassFilesystemFullSize, 32, 32},
		{wire.ClassFilesystemDevice, 8, 8},
		{wire.ClassFilesystemAttribute, 12, 24},
	} {
		full := queryData(t, f.query(t, open, wire.InfoFilesystem, uint8(test.class), test.full), smb.StatusSuccess)
		for _, length := range []uint32{0, test.minimum - 1} {
			message := f.query(t, open, wire.InfoFilesystem, uint8(test.class), length)
			if message.Header.Status != smb.StatusInfoLengthMismatch {
				t.Fatalf("class %d length %d: status = %x", test.class, length, message.Header.Status)
			}
			if _, err := wire.DecodeErrorResponse(message); err != nil {
				t.Fatal(err)
			}
			f.echo(t)
		}
		if test.minimum < test.full {
			for _, length := range []uint32{test.minimum, test.minimum + 1, test.full - 1} {
				data := queryData(t, f.query(t, open, wire.InfoFilesystem, uint8(test.class), length), smb.StatusBufferOverflow)
				if !bytes.Equal(data, full[:length]) {
					t.Fatalf("class %d length %d: prefix = %x, want %x", test.class, length, data, full[:length])
				}
				f.echo(t)
			}
		}
		data := queryData(t, f.query(t, open, wire.InfoFilesystem, uint8(test.class), test.full+1), smb.StatusSuccess)
		if !bytes.Equal(data, full) {
			t.Fatalf("class %d: output changed with larger buffer", test.class)
		}
	}
}

func TestFilesystemStatFSFailureKeepsConnection(t *testing.T) {
	storage := statFSFaultStorage{Storage: newFilesMetaStorage(t), err: smb.ErrIO}
	f := newQueryInfoFixtureWithStorage(t, storage)
	_, open := f.create(t, "fs-fault", smb.KindFile)
	for _, class := range []wire.FilesystemInfoClass{wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize} {
		message := f.query(t, open, wire.InfoFilesystem, uint8(class), 1024)
		if message.Header.Status != smb.StatusIODeviceError {
			t.Fatalf("class %d StatFS failure: status = %x", class, message.Header.Status)
		}
		if _, err := wire.DecodeErrorResponse(message); err != nil {
			t.Fatal(err)
		}
		f.echo(t)
	}
	// These two classes need no space data, so a StatFS outage does not block them.
	for _, class := range []wire.FilesystemInfoClass{wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute} {
		queryData(t, f.query(t, open, wire.InfoFilesystem, uint8(class), 1024), smb.StatusSuccess)
		f.echo(t)
	}
}

func TestUnsupportedFilesystemQueriesKeepConnection(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, file := f.create(t, "fs-query", smb.KindFile)
	root, err := f.storage.Lookup(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	directory := f.open(t, root.Object)
	for _, open := range []state.Open{file, directory} {
		for class := uint16(0); class <= 255; class++ {
			switch wire.FilesystemInfoClass(class) {
			case wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize, wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute:
				continue
			}
			want := smb.StatusInvalidInfoClass
			if class >= 1 && class <= 11 {
				want = smb.StatusNotSupported
			}
			message := f.query(t, open, wire.InfoFilesystem, uint8(class), 1024)
			if message.Header.Status != want {
				t.Fatalf("class %d: status = %x, want %x", class, message.Header.Status, want)
			}
			if _, err := wire.DecodeErrorResponse(message); err != nil {
				t.Fatal(err)
			}
			f.echo(t)
		}
	}
}
