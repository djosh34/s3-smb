package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFilesystemSizeOffsets(t *testing.T) {
	// Distinct counts expose shifted writes and caller/actual swaps (#109).
	space := smb.Space{Capacity: 91*4096 + 4095, Available: 23*4096 + 1, Free: 47*4096 + 511}
	for _, class := range []wire.FilesystemInfoClass{wire.ClassFilesystemSize, wire.ClassFilesystemFullSize} {
		data, err := encodeFilesystemInfo(class, "backup", space)
		if err != nil {
			t.Fatal(err)
		}
		if got := binary.LittleEndian.Uint64(data[0:8]); got != 91 {
			t.Fatalf("class %d total at offset 0 = %d, want 91", class, got)
		}
		if got := binary.LittleEndian.Uint64(data[8:16]); got != 23 {
			t.Fatalf("class %d available at offset 8 = %d, want 23", class, got)
		}
		sectorOffset := 16
		if class == wire.ClassFilesystemFullSize {
			if got := binary.LittleEndian.Uint64(data[16:24]); got != 47 {
				t.Fatalf("actual available at offset 16 = %d, want 47", got)
			}
			sectorOffset = 24
		}
		if got := binary.LittleEndian.Uint32(data[sectorOffset : sectorOffset+4]); got != 8 {
			t.Fatalf("class %d sectors per unit = %d, want 8", class, got)
		}
		if got := binary.LittleEndian.Uint32(data[sectorOffset+4 : sectorOffset+8]); got != 512 {
			t.Fatalf("class %d bytes per sector = %d, want 512", class, got)
		}
		if len(data) != sectorOffset+8 {
			t.Fatalf("class %d length = %d", class, len(data))
		}
	}
}

func TestFilesystemAllocationUnitBoundaries(t *testing.T) {
	for _, size := range []uint64{0, 4095, 4096, math.MaxUint64} {
		data, err := encodeFilesystemInfo(wire.ClassFilesystemFullSize, "", smb.Space{Capacity: size, Available: size, Free: size})
		if err != nil {
			t.Fatal(err)
		}
		for _, offset := range []int{0, 8, 16} {
			if got := binary.LittleEndian.Uint64(data[offset : offset+8]); got != size/4096 {
				t.Fatalf("size %d offset %d = %d, want %d", size, offset, got, size/4096)
			}
		}
	}
}

func TestFilesystemVolumeAndDeviceInformation(t *testing.T) {
	storage := newFilesMetaStorage(t)
	space, err := storage.StatFS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := RequestContext{Storage: storage, Tree: Tree{Share: "backup\U0001f600"}}
	data, status := queryFilesystemInfo(t.Context(), request, uint8(wire.ClassFilesystemVolume), 1024)
	if status != smb.StatusSuccess {
		t.Fatalf("volume status = %x", status)
	}
	volume, err := wire.DecodeFilesystemVolumeInformation(data)
	if err != nil {
		t.Fatal(err)
	}
	if volume.Label != request.Tree.Share || uint64(volume.Serial) != space.VolumeID&0xffffffff || volume.Created != 0 || volume.SupportsObjects {
		t.Fatalf("volume = %+v; space = %+v", volume, space)
	}
	data, status = queryFilesystemInfo(t.Context(), request, uint8(wire.ClassFilesystemDevice), 8)
	if status != smb.StatusSuccess {
		t.Fatalf("device status = %x", status)
	}
	device, err := wire.DecodeFilesystemDeviceInformation(data)
	if err != nil {
		t.Fatal(err)
	}
	if device.Type != 7 || device.Characteristics != 0x10 {
		t.Fatalf("device = %+v", device)
	}
}

func TestFilesystemShortFixedBuffers(t *testing.T) {
	for class, minimum := range map[wire.FilesystemInfoClass]uint32{
		wire.ClassFilesystemVolume: 18, wire.ClassFilesystemSize: 24, wire.ClassFilesystemFullSize: 32,
		wire.ClassFilesystemDevice: 8, wire.ClassFilesystemAttribute: 12,
	} {
		for _, length := range []uint32{0, minimum - 1} {
			data, status := queryFilesystemInfo(t.Context(), RequestContext{}, uint8(class), length)
			if status != smb.StatusInfoLengthMismatch || data != nil {
				t.Fatalf("class %d length %d: data/status = %x/%x", class, length, data, status)
			}
		}
	}
}

func TestFilesystemUnsupportedClasses(t *testing.T) {
	for class := uint16(0); class <= 255; class++ {
		switch wire.FilesystemInfoClass(class) {
		case wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize, wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute:
			continue
		}
		want := smb.StatusInvalidInfoClass
		if class >= 1 && class <= 11 {
			want = smb.StatusNotSupported
		}
		data, status := queryFilesystemInfo(t.Context(), RequestContext{}, uint8(class), 1024)
		if status != want || data != nil {
			t.Fatalf("class %d: data/status = %x/%x, want nil/%x", class, data, status, want)
		}
	}
}

func TestFilesystemStatFSError(t *testing.T) {
	storage := newFilesMetaStorage(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, class := range []wire.FilesystemInfoClass{wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize} {
		data, status := queryFilesystemInfo(ctx, RequestContext{Storage: storage}, uint8(class), 1024)
		if status != smb.StatusCancelled || data != nil {
			t.Fatalf("class %d: data/status = %x/%x", class, data, status)
		}
	}
}

func TestFilesystemAttributeInformation(t *testing.T) {
	data, status := queryFilesystemInfo(t.Context(), RequestContext{}, uint8(wire.ClassFilesystemAttribute), 1024)
	if status != smb.StatusSuccess {
		t.Fatalf("status = %x", status)
	}
	// Read the mask directly at the MS-FSCC offset, not through a paired codec.
	if got := binary.LittleEndian.Uint32(data[0:4]); got != smb.AdvertisedFilesystemAttributes {
		t.Fatalf("attributes = %x, want %x", got, smb.AdvertisedFilesystemAttributes)
	}
	info, err := wire.DecodeFilesystemAttributeInformation(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "s3-smb" || info.MaxComponentLength != 255 {
		t.Fatalf("attribute information = %+v", info)
	}
}

func TestFilesystemAttributeOutputLength(t *testing.T) {
	full, status := queryFilesystemInfo(t.Context(), RequestContext{}, uint8(wire.ClassFilesystemAttribute), 1024)
	if status != smb.StatusSuccess {
		t.Fatalf("full status = %x", status)
	}
	for _, length := range []uint32{0, 11, 12, 13, 23, 24, 25} {
		data, status := queryFilesystemInfo(t.Context(), RequestContext{}, uint8(wire.ClassFilesystemAttribute), length)
		switch {
		case length < 12:
			if status != smb.StatusInfoLengthMismatch || data != nil {
				t.Fatalf("length %d: data/status = %x/%x", length, data, status)
			}
		case uint64(length) < uint64(len(full)):
			if status != smb.StatusBufferOverflow || !bytes.Equal(data, full[:length]) {
				t.Fatalf("length %d: data/status = %x/%x", length, data, status)
			}
		default:
			if status != smb.StatusSuccess || !bytes.Equal(data, full) {
				t.Fatalf("length %d: data/status = %x/%x", length, data, status)
			}
		}
	}
}
