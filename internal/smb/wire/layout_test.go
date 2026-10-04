package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// These tests check encoded field offsets against MS-SMB2 and MS-FSCC, which
// a round trip through a matching decoder cannot catch.

func TestInformationFieldOffsets(t *testing.T) {
	size, err := EncodeFilesystemSizeInformation(FilesystemSizeInformation{TotalUnits: 0x0102030405060708, AvailableUnits: 0x1112131415161718, SectorsPerUnit: 0x21222324, BytesPerSector: 0x31323334})
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte{8, 7, 6, 5, 4, 3, 2, 1, 24, 23, 22, 21, 20, 19, 18, 17, 36, 35, 34, 33, 52, 51, 50, 49}
	if !bytes.Equal(size, expected) {
		t.Fatalf("FsSize: %x", size)
	}
	full, err := EncodeFilesystemFullSizeInformation(FilesystemFullSizeInformation{TotalUnits: 1, CallerAvailableUnits: 2, ActualAvailableUnits: 3, SectorsPerUnit: 4, BytesPerSector: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 32 || binary.LittleEndian.Uint64(full[16:]) != 3 || binary.LittleEndian.Uint32(full[24:]) != 4 || binary.LittleEndian.Uint32(full[28:]) != 5 {
		t.Fatalf("FsFullSize: %x", full)
	}
	attribute, err := EncodeFilesystemAttributeInformation(FilesystemAttributeInformation{MaxComponentLength: -1})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(attribute[4:]) != 0xffffffff {
		t.Fatalf("FsAttribute signed length: %x", attribute)
	}
	all, err := EncodeFileAllInformation(sampleFileAll)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		offset int
		value  uint32
	}{{32, 5}, {56, 8}, {72, 10}, {76, 11}, {80, 12}, {88, 13}, {92, 14}} {
		if binary.LittleEndian.Uint32(all[field.offset:]) != field.value {
			t.Fatalf("FileAll field at %d: %x", field.offset, all)
		}
	}
	if !bytes.Equal(all[96:], []byte{12, 0, 0, 0, 'n', 0, 'a', 0, 'm', 0, 'e', 0, 0x3d, 0xd8, 0, 0xde}) {
		t.Fatalf("FileAll name: %x", all[96:])
	}
	both, err := EncodeDirectoryIDBothEntries([]DirectoryIDBothEntry{{Name: "one😀", ShortName: "ONE", Metadata: sampleMeta}})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 114 || binary.LittleEndian.Uint32(both[60:]) != 10 || both[68] != 6 || !bytes.Equal(both[70:76], []byte{'O', 0, 'N', 0, 'E', 0}) || binary.LittleEndian.Uint64(both[96:]) != 8 || !bytes.Equal(both[104:108], []byte{'o', 0, 'n', 0}) {
		t.Fatalf("FileIdBoth: %x", both)
	}
	fullDirectory, err := EncodeDirectoryIDFullEntries([]DirectoryIDFullEntry{{Name: "one😀", Metadata: sampleMeta}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fullDirectory) != 90 || binary.LittleEndian.Uint64(fullDirectory[72:]) != 8 || !bytes.Equal(fullDirectory[80:84], []byte{'o', 0, 'n', 0}) {
		t.Fatalf("FileIdFull: %x", fullDirectory)
	}
}

func TestBasicDirectoryLayouts(t *testing.T) {
	for _, test := range []struct {
		encode       func([]DirectoryEntry) ([]byte, error)
		class        DirectoryInfoClass
		fixed        int
		lengthOffset int
	}{
		{EncodeDirectoryEntries, ClassDirectory, 64, 60},
		{EncodeDirectoryFullEntries, ClassDirectoryFull, 68, 60},
		{EncodeDirectoryBothEntries, ClassDirectoryBoth, 94, 60},
		{EncodeDirectoryNamesEntries, ClassDirectoryNames, 12, 8},
	} {
		data, err := test.encode(sampleDirectory(test.class))
		if err != nil {
			t.Fatal(err)
		}
		// The first name is six bytes, so the next entry starts at the
		// following eight-byte boundary and the padding is zero.
		end := test.fixed + 6
		next := (end + 7) &^ 7
		if int(binary.LittleEndian.Uint32(data)) != next || binary.LittleEndian.Uint32(data[next:]) != 0 {
			t.Fatalf("class %d links: %x", test.class, data)
		}
		if binary.LittleEndian.Uint32(data[test.lengthOffset:]) != 6 || !bytes.Equal(data[test.fixed:end], []byte{'a', 0, 0x3d, 0xd8, 0, 0xde}) {
			t.Fatalf("class %d name: %x", test.class, data)
		}
		if !bytes.Equal(data[end:next], make([]byte, next-end)) {
			t.Fatalf("class %d padding: %x", test.class, data)
		}
		if test.class != ClassDirectoryNames && binary.LittleEndian.Uint64(data[40:]) != 101 {
			t.Fatalf("class %d end of file: %x", test.class, data)
		}
	}
}

func TestCommandFieldOffsets(t *testing.T) {
	create, err := EncodeCreateRequest(CreateRequest{Name: "dir/😀", Contexts: []CreateContext{{Name: "AAPL", Data: []byte{1, 2, 3}}}})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(create) != 57 || binary.LittleEndian.Uint16(create[44:]) != 120 || binary.LittleEndian.Uint16(create[46:]) != 12 || binary.LittleEndian.Uint32(create[48:]) != 136 {
		t.Fatalf("CREATE offsets: %x", create)
	}
	write, err := EncodeWriteRequest(WriteRequest{Data: []byte{1, 2, 3}, ChannelInfo: []byte{4, 5}, Flags: 11})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(write) != 49 || binary.LittleEndian.Uint16(write[2:]) != 112 || binary.LittleEndian.Uint32(write[4:]) != 3 || binary.LittleEndian.Uint16(write[40:]) != 115 || binary.LittleEndian.Uint16(write[42:]) != 2 || binary.LittleEndian.Uint32(write[44:]) != 11 {
		t.Fatalf("WRITE offsets: %x", write)
	}
	request, err := EncodeNegotiateRequest(NegotiateRequest{Dialects: []uint16{0x311, 0x210}, Contexts: []NegotiateContext{{Type: 1, Data: []byte{1, 0, 2, 0, 1, 0, 3, 4}}, {Type: 0xffff, Data: []byte{5}}}})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(request[28:]) != 104 || binary.LittleEndian.Uint16(request[32:]) != 2 || binary.LittleEndian.Uint16(request[36:]) != 0x311 || binary.LittleEndian.Uint16(request[40:]) != 1 || binary.LittleEndian.Uint16(request[56:]) != 0xffff {
		t.Fatalf("NEGOTIATE request offsets: %x", request)
	}
	response, err := EncodeNegotiateResponse(NegotiateResponse{Token: []byte{1, 2, 3}, Contexts: []NegotiateContext{{Type: 8, Data: []byte{1, 0, 2, 0}}}, Dialect: 0x311})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(response[56:]) != 128 || binary.LittleEndian.Uint16(response[58:]) != 3 || binary.LittleEndian.Uint32(response[60:]) != 136 || !bytes.Equal(response[64:67], []byte{1, 2, 3}) || binary.LittleEndian.Uint16(response[72:]) != 8 {
		t.Fatalf("NEGOTIATE response offsets: %x", response)
	}
	lease, err := EncodeLeaseBreakNotification(LeaseBreakNotification{CurrentState: 3, NewState: 4, Epoch: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(lease) != 44 || binary.LittleEndian.Uint16(lease) != 44 || binary.LittleEndian.Uint16(lease[2:]) != 9 || binary.LittleEndian.Uint32(lease[24:]) != 3 || binary.LittleEndian.Uint32(lease[28:]) != 4 {
		t.Fatalf("lease notification: %x", lease)
	}
	setInfo, err := EncodeSetInfoResponse(EmptyResponse{})
	if err != nil || !bytes.Equal(setInfo, []byte{2, 0}) {
		t.Fatalf("SET_INFO response: %x, %v", setInfo, err)
	}
}

// MS-SMB2 3.3.5.3.1 and 3.3.5.4 place the security buffer after the 64-byte
// header and 64-byte fixed body, even when the buffer is empty.
func TestNegotiateResponseEmptySecurityBufferOffset(t *testing.T) {
	for _, dialect := range []uint16{0x02ff, 0x0311} {
		encoded, err := EncodeNegotiateResponse(NegotiateResponse{Dialect: dialect})
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) != 64 || binary.LittleEndian.Uint16(encoded[56:]) != 128 || binary.LittleEndian.Uint16(encoded[58:]) != 0 {
			t.Fatalf("dialect %#x: %x", dialect, encoded)
		}
	}
}

// A CREATE for the share root has an empty name but still carries the one
// buffer byte that StructureSize 57 counts.
func TestEmptyCreateBufferByte(t *testing.T) {
	data, err := EncodeCreateRequest(CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]byte, 57)
	expected[0] = 57
	if !bytes.Equal(data, expected) {
		t.Fatalf("root CREATE body: %x", data)
	}
	if _, err := DecodeCreateRequest(Message{Header: Header{Command: Create}, Body: expected}); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCreateRequest(Message{Header: Header{Command: Create}, Body: expected[:56]}); err == nil {
		t.Fatal("accepted CREATE without its buffer byte")
	}
}

// Error contexts after the first start on an eight-byte boundary.
func TestErrorContextPadding(t *testing.T) {
	data := []byte{3, 0, 0, 0, 1, 0, 0, 0, 0x11, 0x22, 0x33, 0, 0, 0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 0, 0x44, 0x55}
	encoded, err := EncodeErrorResponse(ErrorResponse{ContextCount: 2, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(encoded[4:]) != 26 || !bytes.Equal(encoded[8:], data) {
		t.Fatalf("error context layout: %x", encoded)
	}
	unpadded := append(clone(data[:11]), data[16:]...)
	if _, err := EncodeErrorResponse(ErrorResponse{ContextCount: 2, Data: unpadded}); err == nil {
		t.Fatal("encoded unaligned error contexts")
	}
	bad := clone(encoded[:8])
	bad[4] = 21 // len(unpadded)
	bad = append(bad, unpadded...)
	if _, err := DecodeErrorResponse(Message{Header: Header{Command: Create, Flags: FlagResponse}, Body: bad}); err == nil {
		t.Fatal("decoded unaligned error contexts")
	}
}
