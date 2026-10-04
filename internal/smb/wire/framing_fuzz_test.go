package wire

import (
	"bytes"
	"testing"
)

func FuzzSplit(f *testing.F) {
	packet, err := Join([]Message{{Header: sampleHeader(), Body: []byte{1, 2, 3}}, {Header: Header{Command: Echo}, Body: []byte{4, 0, 0, 0}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(packet)
	reserved := clone(packet)
	reserved[10] = 0xff
	reserved[11] = 0xee
	f.Add(reserved)
	f.Add([]byte{})
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, packet []byte) {
		members, err := Split(packet)
		if err != nil {
			return
		}
		joined, err := Join(members)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Split(joined)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(members) {
			t.Fatal("compound member count changed")
		}
		for i, member := range members {
			if again[i].Header != member.Header || !bytes.Equal(again[i].Body, member.Body) {
				t.Fatal("decoded compound changed")
			}
		}
	})
}

func FuzzDecodeSMB1Negotiate(f *testing.F) {
	b := builder{}
	b.bytes([]byte{0xff, 'S', 'M', 'B', 0x72})
	b.zero(27)
	b.u8(0)
	dialects := []byte("\x02SMB 2.???\x00")
	b.length16(len(dialects))
	b.bytes(dialects)
	seed, err := b.finish()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, packet []byte) {
		err := DecodeSMB1Negotiate(packet)
		if err == nil && len(packet) < 35 {
			t.Fatal("accepted short SMB1 negotiate")
		}
	})
}

func FuzzDecodeFiletime(f *testing.F) {
	for _, v := range []uint64{0, 1, 116444736000000000, 0xffffffffffffffff, 0xfffffffffffffffe, 0xfffffffffffffffd} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, value uint64) {
		decoded, err := DecodeFiletime(Filetime(value))
		if err != nil {
			return
		}
		encoded, err := EncodeFiletime(decoded)
		if err != nil || uint64(encoded) != value {
			t.Fatalf("FILETIME changed: %x, %v", encoded, err)
		}
	})
}

func FuzzDecodeTimeUpdate(f *testing.F) {
	for _, v := range []uint64{0, 1, 116444736000000000, 0xffffffffffffffff, 0xfffffffffffffffe} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, value uint64) {
		decoded, err := DecodeTimeUpdate(Filetime(value))
		if err != nil {
			return
		}
		encoded, err := EncodeTimeUpdate(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Action == TimeKeep {
			if encoded != FiletimeUnchanged {
				t.Fatal("TimeKeep is not canonical zero")
			}
		} else if uint64(encoded) != value {
			t.Fatal("TimeSet changed")
		}
	})
}
