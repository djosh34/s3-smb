package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestInformationFieldOffsets(t *testing.T) {
	size, err := EncodeFilesystemSizeInformation(FilesystemSizeInformation{TotalUnits: 0x0102030405060708, AvailableUnits: 0x1112131415161718, SectorsPerUnit: 0x21222324, BytesPerSector: 0x31323334})
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte{8, 7, 6, 5, 4, 3, 2, 1, 24, 23, 22, 21, 20, 19, 18, 17, 36, 35, 34, 33, 52, 51, 50, 49}
	if !bytes.Equal(size, expected) {
		t.Fatalf("FsSize: %x", size)
	}
	full, err := EncodeFilesystemFullSizeInformation(sampleFilesystemFullSizeInformation())
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 32 || binary.LittleEndian.Uint64(full[16:]) != 3 || binary.LittleEndian.Uint32(full[24:]) != 4 || binary.LittleEndian.Uint32(full[28:]) != 5 {
		t.Fatalf("FsFullSize: %x", full)
	}
	all, err := EncodeFileAllInformation(sampleFileAllInformation())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		offset int
		value  uint32
	}{{32, 5}, {56, 8}, {72, 10}, {76, 11}, {88, 13}, {92, 14}, {96, 12}} {
		if binary.LittleEndian.Uint32(all[field.offset:]) != field.value {
			t.Fatalf("FileAll field at %d: %x", field.offset, all)
		}
	}
	if binary.LittleEndian.Uint64(all[80:]) != 12 {
		t.Fatal("FileAll position offset")
	}
	if !bytes.Equal(all[100:], []byte{'n', 0, 'a', 0, 'm', 0, 'e', 0, 0x3d, 0xd8, 0, 0xde}) {
		t.Fatal("FileAll name offset")
	}
	both, err := EncodeDirectoryIDBothEntries(sampleDirectoryIDBothEntries()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 114 || binary.LittleEndian.Uint32(both[60:]) != 10 || both[68] != 6 || !bytes.Equal(both[70:76], []byte{'O', 0, 'N', 0, 'E', 0}) || binary.LittleEndian.Uint64(both[96:]) != 8 || !bytes.Equal(both[104:108], []byte{'o', 0, 'n', 0}) {
		t.Fatalf("FileIdBoth: %x", both)
	}
	fullDir, err := EncodeDirectoryIDFullEntries(sampleDirectoryIDFullEntries()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(fullDir) != 90 || binary.LittleEndian.Uint64(fullDir[72:]) != 8 || !bytes.Equal(fullDir[80:84], []byte{'o', 0, 'n', 0}) {
		t.Fatalf("FileIdFull: %x", fullDir)
	}
}

func TestCommandFieldOffsets(t *testing.T) {
	create, err := EncodeCreateRequest(sampleCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(create) < 56 || binary.LittleEndian.Uint16(create) != 57 || binary.LittleEndian.Uint16(create[44:]) != 120 || binary.LittleEndian.Uint16(create[46:]) != 12 || binary.LittleEndian.Uint32(create[48:]) != 136 {
		t.Fatalf("CREATE offsets: %x", create)
	}
	write, err := EncodeWriteRequest(sampleWriteRequest())
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(write) != 49 || binary.LittleEndian.Uint16(write[2:]) != 112 || binary.LittleEndian.Uint32(write[4:]) != 3 || binary.LittleEndian.Uint16(write[40:]) != 115 || binary.LittleEndian.Uint16(write[42:]) != 2 || binary.LittleEndian.Uint32(write[44:]) != 11 {
		t.Fatalf("WRITE offsets: %x", write)
	}
	request, err := EncodeNegotiateRequest(sampleNegotiateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(request[28:]) != 104 || binary.LittleEndian.Uint16(request[32:]) != 2 || binary.LittleEndian.Uint16(request[36:]) != 0x311 || binary.LittleEndian.Uint16(request[40:]) != 1 || binary.LittleEndian.Uint16(request[56:]) != 0xffff {
		t.Fatalf("NEGOTIATE request offsets: %x", request)
	}
	response, err := EncodeNegotiateResponse(sampleNegotiateResponse())
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(response[56:]) != 128 || binary.LittleEndian.Uint16(response[58:]) != 3 || binary.LittleEndian.Uint32(response[60:]) != 136 || !bytes.Equal(response[64:67], []byte{1, 2, 3}) || binary.LittleEndian.Uint16(response[72:]) != 8 {
		t.Fatalf("NEGOTIATE response offsets: %x", response)
	}
	lease, err := EncodeLeaseBreakNotification(sampleLeaseBreakNotification())
	if err != nil {
		t.Fatal(err)
	}
	if len(lease) != 44 || binary.LittleEndian.Uint16(lease) != 44 || binary.LittleEndian.Uint16(lease[2:]) != 9 || binary.LittleEndian.Uint32(lease[24:]) != 3 || binary.LittleEndian.Uint32(lease[28:]) != 4 {
		t.Fatalf("lease notification: %x", lease)
	}
}

func TestContextVariantsAndReservedBytes(t *testing.T) {
	checkRoundTrip(t, LeaseContext{Key: [16]byte{1}, Duration: 2, State: 3, Flags: 4, Version: 1}, func(v LeaseContext) ([]byte, error) { c, err := EncodeLeaseContext(v); return c.Data, err }, func(b []byte) (LeaseContext, error) { return DecodeLeaseContext(CreateContext{Name: "RqLs", Data: b}) })
	for bits := uint64(0); bits < 8; bits++ {
		v := AAPLReply{Returned: bits}
		if bits&1 != 0 {
			v.ServerCapabilities = 1
		}
		if bits&2 != 0 {
			v.VolumeCapabilities = 6
		}
		if bits&4 != 0 {
			v.Model = "Mac"
		}
		checkRoundTrip(t, v, func(v AAPLReply) ([]byte, error) { c, err := EncodeAAPLReply(v); return c.Data, err }, func(b []byte) (AAPLReply, error) { return DecodeAAPLReply(CreateContext{Name: "AAPL", Data: b}) })
	}
	if v, err := DecodeMaxAccessQuery(CreateContext{Name: "MxAc"}); err != nil || v.Timestamp != 0 {
		t.Fatalf("empty MxAc query: %+v, %v", v, err)
	}
	encoded, err := EncodeLeaseBreakRequest(sampleLeaseBreakRequest())
	if err != nil {
		t.Fatal(err)
	}
	encoded[2] = 0xfe
	encoded[3] = 0xff
	decoded, err := DecodeLeaseBreakRequest(Message{Header: Header{Command: OplockBreak}, Body: encoded})
	if err != nil {
		t.Fatal(err)
	}
	again, err := EncodeLeaseBreakRequest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if again[2] != 0 || again[3] != 0 {
		t.Fatal("acknowledgment reserved bytes were not zeroed")
	}
	checkRoundTrip(t, NegotiateRequest{Dialects: []uint16{0x210}}, EncodeNegotiateRequest, func(b []byte) (NegotiateRequest, error) {
		return DecodeNegotiateRequest(Message{Header: Header{Command: Negotiate}, Body: b})
	})
	checkRoundTrip(t, NegotiateResponse{Dialect: 0x2ff}, EncodeNegotiateResponse, func(b []byte) (NegotiateResponse, error) {
		return DecodeNegotiateResponse(Message{Header: Header{Command: Negotiate, Flags: FlagResponse}, Body: b})
	})
}
