package wire

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestMalformedVariableFields(t *testing.T) {
	original, err := EncodeWriteRequest(sampleWriteRequest())
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
		b := clone(original)
		change(b)
		if _, decodeErr := DecodeWriteRequest(Message{Header: Header{Command: Write}, Body: b}); decodeErr == nil {
			t.Fatal("accepted malformed write buffer")
		}
	}
	create, err := EncodeCreateRequest(sampleCreateRequest())
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
}

func TestUTF16Validation(t *testing.T) {
	for _, bad := range [][]byte{{1}, {0, 0xd8}, {0, 0xdc}, {0, 0xd8, 65, 0}, {0, 0xdc, 0, 0xd8}} {
		b := builder{}
		b.length32(len(bad))
		b.bytes(bad)
		if _, err := DecodeFileNameInformation(b.data); err == nil {
			t.Fatalf("accepted UTF-16 %x", bad)
		}
		body := builder{}
		body.u16(9)
		body.u16(0)
		body.u16(72)
		body.length16(len(bad))
		body.bytes(bad)
		if _, err := DecodeTreeConnectRequest(Message{Header: Header{Command: TreeConnect}, Body: body.data}); err == nil {
			t.Fatal("accepted malformed tree path")
		}
	}
	if _, err := EncodeFileNameInformation(FileNameInformation{Name: string([]byte{0xff})}); err == nil {
		t.Fatal("accepted malformed UTF-8")
	}
}

func TestCreateContextChainValidation(t *testing.T) {
	original, err := encodeCreateContexts([]CreateContext{{Name: "AAPL", Data: []byte{1, 2}}, {Name: "MxAc", Data: []byte{3, 4}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b, 8) },
		func(b []byte) { binary.LittleEndian.PutUint32(b, 25) },
		func(b []byte) { binary.LittleEndian.PutUint32(b, 0xfffffff8) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[4:], 8) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[6:], 0xffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[10:], 16) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[12:], 0xffffffff) },
		func(b []byte) { b[32+6] = 0xff; b[32+7] = 0xff },
	} {
		b := clone(original)
		change(b)
		if contexts, err := decodeCreateContexts(b); err == nil || contexts != nil {
			t.Fatal("accepted a malformed context chain")
		}
	}
}

func TestNegotiateContextValidation(t *testing.T) {
	original, err := EncodeNegotiateRequest(sampleNegotiateRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 100) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 105) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[32:], 0xffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[42:], 0xffff) },
	} {
		b := clone(original)
		change(b)
		if _, err := DecodeNegotiateRequest(Message{Header: Header{Command: Negotiate}, Body: b}); err == nil {
			t.Fatal("accepted malformed negotiate contexts")
		}
	}
}

func TestLinkedInformationValidation(t *testing.T) {
	stream, err := EncodeFileStreamInformation(sampleFileStreamInformation())
	if err != nil {
		t.Fatal(err)
	}
	both, err := EncodeDirectoryIDBothEntries(sampleDirectoryIDBothEntries())
	if err != nil {
		t.Fatal(err)
	}
	full, err := EncodeDirectoryIDFullEntries(sampleDirectoryIDFullEntries())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		decode       func([]byte) error
		data         []byte
		lengthOffset int
	}{
		{data: stream, decode: func(b []byte) error { _, err := DecodeFileStreamInformation(b); return err }, lengthOffset: 4},
		{data: both, decode: func(b []byte) error { _, err := DecodeDirectoryIDBothEntries(b); return err }, lengthOffset: 60},
		{data: full, decode: func(b []byte) error { _, err := DecodeDirectoryIDFullEntries(b); return err }, lengthOffset: 60},
	}
	for _, c := range cases {
		for _, offset := range []uint32{1, 8, 0xfffffff8} {
			bad := clone(c.data)
			binary.LittleEndian.PutUint32(bad, offset)
			if err := c.decode(bad); err == nil {
				t.Fatalf("accepted link %d", offset)
			}
		}
		bad := clone(c.data)
		binary.LittleEndian.PutUint32(bad[c.lengthOffset:], 0xffffffff)
		if err := c.decode(bad); err == nil {
			t.Fatal("accepted a name outside its member")
		}
		next := int(binary.LittleEndian.Uint32(c.data))
		bad = clone(c.data)
		binary.LittleEndian.PutUint32(bad[next+c.lengthOffset:], 0xffffffff)
		if err := c.decode(bad); err == nil {
			t.Fatal("accepted a malformed suffix")
		}
	}
	for _, n := range []byte{1, 25} {
		bad := clone(both)
		bad[68] = n
		if _, err := DecodeDirectoryIDBothEntries(bad); err == nil {
			t.Fatal("accepted bad short-name length")
		}
	}
}

func TestSecurityValidationAndPresence(t *testing.T) {
	for _, control := range []uint16{DescriptorSelfRelative, DescriptorSelfRelative | DACLPresent, DescriptorSelfRelative | DACLPresent | SACLPresent} {
		checkRoundTrip(t, SecurityDescriptor{Revision: 1, Control: control}, EncodeSecurityDescriptor, DecodeSecurityDescriptor)
	}
	checkRoundTrip(t, SecurityDescriptor{Revision: 1, Control: DescriptorSelfRelative | DACLPresent, DACL: &ACL{Revision: 2}}, EncodeSecurityDescriptor, DecodeSecurityDescriptor)
	original, err := EncodeSecurityDescriptor(sampleSecurityDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(b []byte) { b[0] = 2 },
		func(b []byte) { binary.LittleEndian.PutUint16(b[2:], 0) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 4) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 21) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 20) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 0xfffffffc) },
		func(b []byte) { b[21] = 16 },
	} {
		b := clone(original)
		change(b)
		if _, decodeErr := DecodeSecurityDescriptor(b); decodeErr == nil {
			t.Fatal("accepted malformed descriptor")
		}
	}
	acl, err := EncodeACL(sampleACL())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){func(b []byte) { b[8] = 5 }, func(b []byte) { b[10] = 4 }, func(b []byte) { b[4] = 0xff }, func(b []byte) { b[2] = 0xff }} {
		bad := clone(acl)
		change(bad)
		if _, err := DecodeACL(bad); err == nil {
			t.Fatal("accepted malformed ACL")
		}
	}
	if _, err := EncodeSecurityDescriptor(SecurityDescriptor{Revision: 1, Control: DescriptorSelfRelative, DACL: &ACL{Revision: 2}}); err == nil {
		t.Fatal("accepted an ACL without its presence bit")
	}
}

func TestFiletimeAndTimeUpdate(t *testing.T) {
	for _, sentinel := range []Filetime{FiletimeUnchanged, FiletimeSuppress, FiletimeResume} {
		got, err := DecodeTimeUpdate(sentinel)
		if err != nil || got.Action != TimeKeep || !got.Time.IsZero() {
			t.Fatalf("sentinel %x: %+v, %v", sentinel, got, err)
		}
	}
	epoch := time.Unix(0, 0).UTC()
	ticks, err := EncodeFiletime(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 116444736000000000 {
		t.Fatalf("Unix epoch is %d", ticks)
	}
	update, err := DecodeTimeUpdate(ticks)
	if err != nil || update.Action != TimeSet || !update.Time.Equal(epoch) {
		t.Fatalf("epoch: %+v, %v", update, err)
	}
	zero, err := DecodeFiletime(0)
	if err != nil || zero.Year() != 1601 {
		t.Fatalf("FILETIME epoch: %v, %v", zero, err)
	}
	for _, v := range []Filetime{1, Filetime(0x7fffffffffffffff), Filetime(0xfffffffffffffffd)} {
		decoded, err := DecodeFiletime(v)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := EncodeFiletime(decoded)
		if err != nil || encoded != v {
			t.Fatalf("time %x: %x, %v", v, encoded, err)
		}
	}
	for _, v := range []Filetime{FiletimeSuppress, FiletimeResume} {
		if _, err := DecodeFiletime(v); err == nil {
			t.Fatal("converted sentinel")
		}
	}
	if _, err := EncodeFiletime(time.Date(1600, 12, 31, 23, 59, 59, 0, time.UTC)); err == nil {
		t.Fatal("accepted pre-FILETIME date")
	}
	if _, err := EncodeFiletime(time.Date(70000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("accepted overflowing FILETIME")
	}
	if _, err := EncodeTimeUpdate(TimeUpdate{Action: TimeSet, Time: zero}); err == nil {
		t.Fatal("TimeSet collided with zero sentinel")
	}
	if _, err := EncodeTimeUpdate(TimeUpdate{Action: TimeUpdateAction(2)}); err == nil {
		t.Fatal("accepted unknown action")
	}
}
