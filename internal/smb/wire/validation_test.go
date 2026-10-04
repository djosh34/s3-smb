package wire

import (
	"encoding/binary"
	"reflect"
	"testing"
	"time"
)

func TestBodyEnvelope(t *testing.T) {
	data, err := EncodeCloseRequest(CloseRequest{ID: FileID{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	wrongSize := clone(data)
	wrongSize[0] = 0
	for _, m := range []Message{
		{Header: Header{Command: Flush}, Body: data},
		{Header: Header{Command: Close, Flags: FlagResponse}, Body: data},
		{Header: Header{Command: Close}, Body: data[:23]},
		{Header: Header{Command: Close}, Body: wrongSize},
	} {
		if _, err := DecodeCloseRequest(m); err == nil {
			t.Fatalf("accepted %+v", m)
		}
	}
}

func TestMalformedVariableFields(t *testing.T) {
	write, err := EncodeWriteRequest(WriteRequest{Data: []byte{1, 2, 3}, ChannelInfo: []byte{4, 5}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint16(b[2:], 63) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[2:], 110) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 0xffffffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[40:], 112) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[42:], 0xffff) },
	} {
		b := clone(write)
		change(b)
		if _, decodeErr := DecodeWriteRequest(Message{Header: Header{Command: Write}, Body: b}); decodeErr == nil {
			t.Fatalf("accepted malformed write buffer %x", b)
		}
	}
	create, err := EncodeCreateRequest(CreateRequest{Name: "x", Contexts: []CreateContext{{Name: "AAPL", Data: []byte{1}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []uint32{63, 120, 121, 0xfffffff8} {
		bad := clone(create)
		binary.LittleEndian.PutUint32(bad[48:], offset)
		if _, err := DecodeCreateRequest(Message{Header: Header{Command: Create}, Body: bad}); err == nil {
			t.Fatalf("accepted context offset %d", offset)
		}
	}
	// Move the whole name so only its eight-byte alignment is wrong.
	for _, offset := range []uint16{122, 124, 126} {
		bad := append(clone(create[:56]), make([]byte, int(offset)-120)...)
		bad = append(bad, create[56:58]...)
		binary.LittleEndian.PutUint16(bad[44:], offset)
		binary.LittleEndian.PutUint32(bad[48:], 0)
		binary.LittleEndian.PutUint32(bad[52:], 0)
		if _, err := DecodeCreateRequest(Message{Header: Header{Command: Create}, Body: bad}); err == nil {
			t.Fatalf("accepted CREATE name at %d", offset)
		}
	}
}

func TestUTF16Validation(t *testing.T) {
	for _, bad := range [][]byte{{1}, {0, 0xd8}, {0, 0xdc}, {0, 0xd8, 65, 0}, {0, 0xdc, 0, 0xd8}} {
		b := builder{}
		b.length32(len(bad))
		b.bytes(bad)
		if _, err := DecodeFileNameInformation(b.data); err == nil {
			t.Fatalf("accepted UTF-16 %x", bad)
		}
	}
	if _, err := EncodeFileNameInformation(FileNameInformation{Name: string([]byte{0xff})}); err == nil {
		t.Fatal("accepted malformed UTF-8")
	}
}

func TestCreateContextChainValidation(t *testing.T) {
	original, err := encodeCreateContexts([]CreateContext{{Name: "AAPL", Data: []byte{1, 2}}, {Name: "DH2Q", Data: []byte{3, 4}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b, 0) },
		func(b []byte) { binary.LittleEndian.PutUint32(b, 8) },
		func(b []byte) { binary.LittleEndian.PutUint32(b, 25) },
		func(b []byte) { binary.LittleEndian.PutUint32(b, 0xfffffff8) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[4:], 8) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[4:], 18) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[6:], 0xffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[10:], 16) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[12:], 0xffffffff) },
		func(b []byte) { b[32+6] = 0xff; b[32+7] = 0xff },
	} {
		b := clone(original)
		change(b)
		if contexts, err := decodeCreateContexts(b); err == nil || contexts != nil {
			t.Fatalf("accepted a malformed context chain %x", b)
		}
	}
}

// A context without data may carry any DataOffset; clients leave it unset.
func TestUnusedCreateContextDataOffset(t *testing.T) {
	want := []CreateContext{{Name: "QFid"}}
	data, err := encodeCreateContexts(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []uint16{1, 3, 16, 0xffff} {
		changed := clone(data)
		binary.LittleEndian.PutUint16(changed[10:], offset)
		got, err := decodeCreateContexts(changed)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("unused DataOffset %d: %+v, %v", offset, got, err)
		}
	}
}

func TestNegotiateContextValidation(t *testing.T) {
	original, err := EncodeNegotiateRequest(NegotiateRequest{Dialects: []uint16{0x311, 0x210}, Contexts: []NegotiateContext{{Type: 1, Data: []byte{1, 0, 2, 0, 1, 0, 3, 4}}, {Type: 0xffff, Data: []byte{5}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 100) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 105) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[32:], 1) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[32:], 0xffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[42:], 0xffff) },
	} {
		b := clone(original)
		change(b)
		if _, err := DecodeNegotiateRequest(Message{Header: Header{Command: Negotiate}, Body: b}); err == nil {
			t.Fatalf("accepted malformed negotiate contexts %x", b)
		}
	}
}

func TestLinkedListValidation(t *testing.T) {
	stream, err := EncodeFileStreamInformation(FileStreamInformation{Entries: []FileStreamEntry{{Name: "::$DATA"}, {Name: ":x:$DATA"}}})
	if err != nil {
		t.Fatal(err)
	}
	both, err := EncodeDirectoryIDBothEntries([]DirectoryIDBothEntry{{Name: "one"}, {Name: "two"}})
	if err != nil {
		t.Fatal(err)
	}
	names, err := EncodeDirectoryNamesEntries([]DirectoryEntry{{Name: "one"}, {Name: "two"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		decode       func([]byte) error
		data         []byte
		lengthOffset int
	}{
		{func(b []byte) error { _, err := DecodeFileStreamInformation(b); return err }, stream, 4},
		{func(b []byte) error { _, err := DecodeDirectoryIDBothEntries(b); return err }, both, 60},
		{func(b []byte) error { _, err := DecodeDirectoryNamesEntries(b); return err }, names, 8},
	} {
		for _, link := range []uint32{0, 1, 7, 8, 0xfffffff8} {
			bad := clone(test.data)
			binary.LittleEndian.PutUint32(bad, link)
			if err := test.decode(bad); err == nil {
				t.Fatalf("accepted link %d", link)
			}
		}
		bad := clone(test.data)
		binary.LittleEndian.PutUint32(bad[test.lengthOffset:], 0xffffffff)
		if err := test.decode(bad); err == nil {
			t.Fatal("accepted a name outside its entry")
		}
		next := int(binary.LittleEndian.Uint32(test.data))
		bad = clone(test.data)
		binary.LittleEndian.PutUint32(bad[next+test.lengthOffset:], 0xffffffff)
		if err := test.decode(bad); err == nil {
			t.Fatal("accepted a malformed last entry")
		}
		// The last entry may end with up to seven bytes of padding.
		if err := test.decode(append(clone(test.data), make([]byte, 7)...)); err != nil {
			t.Fatalf("rejected padding: %v", err)
		}
		if err := test.decode(append(clone(test.data), make([]byte, 8)...)); err == nil {
			t.Fatal("accepted data after the last entry")
		}
	}
	for _, n := range []byte{1, 25} {
		bad := clone(both)
		bad[68] = n
		if _, err := DecodeDirectoryIDBothEntries(bad); err == nil {
			t.Fatalf("accepted short-name length %d", n)
		}
	}
	if _, err := EncodeDirectoryBothEntries([]DirectoryEntry{{Name: "long", ShortName: "1234567890123"}}); err == nil {
		t.Fatal("accepted a short name over twelve characters")
	}
}

// SET_INFO classes ignore trailing bytes; Samba sends a UTF-16 terminator after
// a rename target that FileNameLength does not count. QUERY_INFO classes must
// match their size exactly.
func TestSetInfoTrailingBytes(t *testing.T) {
	rename := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 'x', 0, 0, 0}
	got, err := DecodeFileRenameInformation(rename)
	if err != nil || got.Name != "x" || !got.ReplaceIfExists {
		t.Fatalf("rename: %+v, %v", got, err)
	}
	disposition, err := DecodeFileDispositionInformation([]byte{0xff, 0, 0, 0})
	if err != nil || !disposition.DeletePending {
		t.Fatalf("disposition: %+v, %v", disposition, err)
	}
	if _, decodeErr := DecodeFileDispositionInformation(nil); decodeErr == nil {
		t.Fatal("accepted a missing disposition flag")
	}
	basic, err := EncodeFileBasicInformation(sampleBasic)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeFileBasicInformation(append(basic, 0, 0)); err != nil || got != sampleBasic {
		t.Fatalf("basic: %+v, %v", got, err)
	}
	if _, err := DecodeFileBasicInformation(basic[:39]); err == nil {
		t.Fatal("accepted a short basic class")
	}
	if _, err := DecodeFileInternalInformation(make([]byte, 9)); err == nil {
		t.Fatal("QUERY_INFO class accepted extra bytes")
	}
}

func TestFiletime(t *testing.T) {
	for _, sentinel := range []Filetime{FiletimeUnchanged, FiletimeSuppress, FiletimeResume} {
		got, err := DecodeTimeUpdate(sentinel)
		if err != nil || got.Action != TimeKeep {
			t.Fatalf("sentinel %x: %+v, %v", sentinel, got, err)
		}
	}
	epoch := time.Unix(0, 0).UTC()
	ticks, err := EncodeFiletime(epoch)
	if err != nil || ticks != 116444736000000000 {
		t.Fatalf("Unix epoch: %d, %v", ticks, err)
	}
	update, err := DecodeTimeUpdate(ticks)
	if err != nil || update.Action != TimeSet || !update.Time.Equal(epoch) {
		t.Fatalf("epoch: %+v, %v", update, err)
	}
	if zero, err := DecodeFiletime(0); err != nil || zero.Year() != 1601 {
		t.Fatalf("FILETIME epoch: %v, %v", zero, err)
	}
	if _, err := EncodeFiletime(time.Date(1600, 12, 31, 23, 59, 59, 0, time.UTC)); err == nil {
		t.Fatal("accepted a date before 1601")
	}
	if _, err := EncodeFiletime(time.Date(70000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("accepted an overflowing FILETIME")
	}
}

func TestContextChainPadding(t *testing.T) {
	create, err := encodeCreateContexts([]CreateContext{{Name: "test", Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	negotiate, err := encodeNegotiateContexts([]NegotiateContext{{Type: 0xffff, Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	for padding, accepted := range map[int]bool{7: true, 8: false} {
		if _, err := decodeCreateContexts(append(clone(create), make([]byte, padding)...)); (err == nil) != accepted {
			t.Fatalf("create contexts with %d padding bytes: %v", padding, err)
		}
		if _, err := decodeNegotiateContexts(append(clone(negotiate), make([]byte, padding)...), 1); (err == nil) != accepted {
			t.Fatalf("negotiate contexts with %d padding bytes: %v", padding, err)
		}
	}
}

func TestContextVariants(t *testing.T) {
	v1 := LeaseContext{Key: [16]byte{1}, Duration: 2, State: 3, Flags: 4, Version: 1}
	lease, err := EncodeLeaseContext(v1)
	if err != nil || len(lease.Data) != 32 {
		t.Fatalf("lease V1: %x, %v", lease.Data, err)
	}
	if got, err := DecodeLeaseContext(lease); err != nil || got != v1 {
		t.Fatalf("lease V1: %+v, %v", got, err)
	}
	// AAPL replies carry only the fields selected by the Returned bitmap.
	for _, want := range []AAPLReply{{}, {Returned: 2, VolumeCapabilities: 6}, {Returned: 4, Model: "Mac"}} {
		reply, err := EncodeAAPLReply(want)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecodeAAPLReply(reply); err != nil || got != want {
			t.Fatalf("AAPL reply: %+v, %v", got, err)
		}
	}
	if _, err := EncodeAAPLReply(AAPLReply{VolumeCapabilities: 6}); err == nil {
		t.Fatal("encoded a field that Returned does not select")
	}
}
