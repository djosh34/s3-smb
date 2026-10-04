package wire

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestFSCCBooleanValues(t *testing.T) {
	standard, err := EncodeFileStandardInformation(sampleFileStandardInformation())
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := EncodeFileDispositionInformation(sampleFileDispositionInformation())
	if err != nil {
		t.Fatal(err)
	}
	rename, err := EncodeFileRenameInformation(sampleFileRenameInformation())
	if err != nil {
		t.Fatal(err)
	}
	volume, err := EncodeFilesystemVolumeInformation(sampleFilesystemVolumeInformation())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		decode func([]byte) (bool, error)
		data   []byte
		offset int
	}{
		{data: standard, offset: 20, decode: func(data []byte) (bool, error) {
			v, err := DecodeFileStandardInformation(data)
			return v.DeletePending, err
		}},
		{data: standard, offset: 21, decode: func(data []byte) (bool, error) {
			v, err := DecodeFileStandardInformation(data)
			return v.Directory, err
		}},
		{data: disposition, offset: 0, decode: func(data []byte) (bool, error) {
			v, err := DecodeFileDispositionInformation(data)
			return v.DeletePending, err
		}},
		{data: rename, offset: 0, decode: func(data []byte) (bool, error) {
			v, err := DecodeFileRenameInformation(data)
			return v.ReplaceIfExists, err
		}},
		{data: volume, offset: 16, decode: func(data []byte) (bool, error) {
			v, err := DecodeFilesystemVolumeInformation(data)
			return v.SupportsObjects, err
		}},
	}
	for _, c := range cases {
		for _, value := range []byte{0, 1, 2, 0xff} {
			data := clone(c.data)
			data[c.offset] = value
			got, decodeErr := c.decode(data)
			if decodeErr != nil || got != (value != 0) {
				t.Fatalf("Boolean %x at %d: %t, %v", value, c.offset, got, decodeErr)
			}
		}
	}
	if _, err := DecodeFileDispositionInformation(nil); err == nil {
		t.Fatal("accepted a missing Boolean")
	}
}

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

func TestCreateNameAlignment(t *testing.T) {
	data, err := EncodeCreateRequest(CreateRequest{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []uint16{122, 124, 126} {
		// Move the whole name so only its alignment is wrong.
		bad := append(clone(data[:56]), make([]byte, int(offset)-120)...)
		bad = append(bad, data[56:]...)
		binary.LittleEndian.PutUint16(bad[44:], offset)
		if _, decodeErr := DecodeCreateRequest(Message{Header: Header{Command: Create}, Body: bad}); decodeErr == nil {
			t.Fatalf("accepted CREATE name at %d", offset)
		}
	}
	contexts, err := encodeCreateContexts([]CreateContext{{Name: "QFid"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []uint16{17, 18, 20} {
		bad := clone(contexts)
		copy(bad[int(offset):], []byte("QFid"))
		binary.LittleEndian.PutUint16(bad[4:], offset)
		if _, decodeErr := decodeCreateContexts(bad); decodeErr == nil {
			t.Fatalf("accepted context name at %d", offset)
		}
	}
}

func TestUnusedCreateContextDataOffset(t *testing.T) {
	expected := []CreateContext{{Name: "QFid"}}
	data, err := encodeCreateContexts(expected)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []uint16{1, 3, 16, 0xffff} {
		changed := clone(data)
		binary.LittleEndian.PutUint16(changed[10:], offset)
		got, decodeErr := decodeCreateContexts(changed)
		if decodeErr != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("unused DataOffset %d: %+v, %v", offset, got, decodeErr)
		}
	}
}

func checkTrailingClass[T any](t *testing.T, value T, encode func(T) ([]byte, error), decode func([]byte) (T, error)) {
	t.Helper()
	data, err := encode(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{0, 2, 7, 16} {
		extended := append(clone(data), make([]byte, extra)...)
		got, decodeErr := decode(extended)
		if decodeErr != nil || !reflect.DeepEqual(got, value) {
			t.Fatalf("%d trailing bytes: %+v, %v", extra, got, decodeErr)
		}
	}
	for n := 0; n < len(data); n++ {
		if _, decodeErr := decode(data[:n]); decodeErr == nil {
			t.Fatalf("accepted a %d-byte class prefix", n)
		}
	}
}

func TestSetInfoTrailingBytes(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		checkTrailingClass(t, sampleFileBasicInformation(), EncodeFileBasicInformation, DecodeFileBasicInformation)
	})
	t.Run("Disposition", func(t *testing.T) {
		checkTrailingClass(t, sampleFileDispositionInformation(), EncodeFileDispositionInformation, DecodeFileDispositionInformation)
	})
	t.Run("EndOfFile", func(t *testing.T) {
		checkTrailingClass(t, sampleFileEndOfFileInformation(), EncodeFileEndOfFileInformation, DecodeFileEndOfFileInformation)
	})
	t.Run("Allocation", func(t *testing.T) {
		checkTrailingClass(t, sampleFileAllocationInformation(), EncodeFileAllocationInformation, DecodeFileAllocationInformation)
	})
	t.Run("Rename", func(t *testing.T) {
		checkTrailingClass(t, FileRenameInformation{Name: "x", ReplaceIfExists: true}, EncodeFileRenameInformation, DecodeFileRenameInformation)
	})
	// Samba sends a trailing UTF-16 terminator that is not in FileNameLength.
	rename := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 'x', 0, 0, 0}
	got, err := DecodeFileRenameInformation(rename)
	if err != nil || got.Name != "x" || !got.ReplaceIfExists {
		t.Fatalf("Samba rename: %+v, %v", got, err)
	}
	disposition, err := DecodeFileDispositionInformation([]byte{1, 0, 0, 0})
	if err != nil || !disposition.DeletePending {
		t.Fatalf("Samba disposition: %+v, %v", disposition, err)
	}
	standard, err := EncodeFileStandardInformation(sampleFileStandardInformation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFileStandardInformation(append(standard, 0)); err == nil {
		t.Fatal("QUERY_INFO Standard accepted extra bytes")
	}
}

func TestReservedRequestHeader(t *testing.T) {
	expected := sampleHeader()
	data, err := EncodeHeader(expected)
	if err != nil {
		t.Fatal(err)
	}
	data[10] = 0xff
	data[11] = 0xee
	got, err := DecodeHeader(data)
	if err != nil || got != expected {
		t.Fatalf("reserved header word: %+v, %v", got, err)
	}
	canonical, err := EncodeHeader(got)
	if err != nil {
		t.Fatal(err)
	}
	if canonical[10] != 0 || canonical[11] != 0 {
		t.Fatal("encoded a nonzero reserved word")
	}
	members, err := Split(append(clone(data), []byte{1, 2, 3}...))
	if err != nil || len(members) != 1 {
		t.Fatalf("reserved word in Split: %v", err)
	}
	if members[0].Raw[10] != 0xff || members[0].Raw[11] != 0xee {
		t.Fatal("Split did not preserve received reserved bytes")
	}
}

func TestErrorContextPadding(t *testing.T) {
	// First context has three data bytes, followed by five padding bytes.
	data := []byte{3, 0, 0, 0, 1, 0, 0, 0, 0x11, 0x22, 0x33, 0, 0, 0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 0, 0x44, 0x55}
	value := ErrorResponse{ContextCount: 2, Data: data}
	encoded, err := EncodeErrorResponse(value)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(encoded[4:]) != 26 || !bytes.Equal(encoded[8:], data) {
		t.Fatalf("error context layout: %x", encoded)
	}
	decoded, err := DecodeErrorResponse(Message{Header: Header{Command: Create, Flags: FlagResponse}, Body: encoded})
	if err != nil || !reflect.DeepEqual(decoded, value) {
		t.Fatalf("padded contexts: %+v, %v", decoded, err)
	}
	unpadded := append(clone(data[:11]), data[16:]...)
	if _, err := EncodeErrorResponse(ErrorResponse{ContextCount: 2, Data: unpadded}); err == nil {
		t.Fatal("encoded unaligned error contexts")
	}
	bad := clone(encoded[:8])
	length, lengthErr := count32(len(unpadded))
	if lengthErr != nil {
		t.Fatal(lengthErr)
	}
	binary.LittleEndian.PutUint32(bad[4:], length)
	bad = append(bad, unpadded...)
	if _, err := DecodeErrorResponse(Message{Header: Header{Command: Create, Flags: FlagResponse}, Body: bad}); err == nil {
		t.Fatal("decoded unaligned error contexts")
	}
}

func TestIOCTLOutputAlignment(t *testing.T) {
	for _, inputLength := range []int{0, 1, 3, 7, 8, 9} {
		value := IOCTLResponse{Input: make([]byte, inputLength), Output: []byte{1, 2, 3}}
		if inputLength == 0 {
			value.Input = nil
		}
		data, err := EncodeIOCTLResponse(value)
		if err != nil {
			t.Fatal(err)
		}
		offset := binary.LittleEndian.Uint32(data[32:])
		want := 112 + inputLength
		want += (8 - want%8) % 8
		if int(offset) != want || !bytes.Equal(data[offset-64:], value.Output) {
			t.Fatalf("input length %d, output offset %d, want %d", inputLength, offset, want)
		}
		if !bytes.Equal(data[48+inputLength:int(offset)-64], make([]byte, want-112-inputLength)) {
			t.Fatal("IOCTL padding is not zero")
		}
		got, err := DecodeIOCTLResponse(Message{Header: Header{Command: IOCTL, Flags: FlagResponse}, Body: data})
		if err != nil || !reflect.DeepEqual(got, value) {
			t.Fatalf("aligned IOCTL: %+v, %v", got, err)
		}
		if inputLength == 3 {
			bad := clone(data)
			binary.LittleEndian.PutUint32(bad[32:], 119)
			if _, decodeErr := DecodeIOCTLResponse(Message{Header: Header{Command: IOCTL, Flags: FlagResponse}, Body: bad}); decodeErr == nil {
				t.Fatal("accepted an unaligned IOCTL output")
			}
		}
	}
}

func TestSplitSharesOwnedMemberBuffer(t *testing.T) {
	packet, err := Join([]Message{{Header: Header{Command: Write}, Body: []byte{1, 2, 3}}, {Header: Header{Command: Echo}, Body: []byte{4, 0, 0, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	members, err := Split(packet)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if &member.Body[0] != &member.Raw[64] {
			t.Fatal("Split copied Body separately from Raw")
		}
	}
	members[0].Body[0] = 9
	if members[0].Raw[64] != 9 || packet[64] != 1 || members[1].Body[0] != 4 {
		t.Fatal("member buffer ownership is wrong")
	}
}
