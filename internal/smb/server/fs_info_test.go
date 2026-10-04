package server

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

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
