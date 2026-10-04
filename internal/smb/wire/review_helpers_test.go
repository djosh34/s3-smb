package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestLengthValidationHelpers(t *testing.T) {
	for _, n := range []int{-1, 0, 255, 256, 65535, 65536} {
		v16, err16 := count16(n)
		fits16 := n >= 0 && n <= 65535
		if (err16 == nil) != fits16 || size16(n) != fits16 {
			t.Fatalf("16-bit length %d: %d, %v", n, v16, err16)
		}
		b16 := builder{}
		b16.length16(n)
		encoded16, buildErr16 := b16.finish()
		if (buildErr16 == nil) != fits16 {
			t.Fatalf("16-bit builder length %d: %v", n, buildErr16)
		}
		if fits16 && (len(encoded16) != 2 || binary.LittleEndian.Uint16(encoded16) != v16) {
			t.Fatal("16-bit builder disagrees with its validator")
		}
		v32, err32 := count32(n)
		fits32 := n >= 0
		if (err32 == nil) != fits32 || size32(n) != fits32 {
			t.Fatalf("32-bit length %d: %d, %v", n, v32, err32)
		}
		b32 := builder{}
		b32.length32(n)
		encoded32, buildErr32 := b32.finish()
		if (buildErr32 == nil) != fits32 {
			t.Fatalf("32-bit builder length %d: %v", n, buildErr32)
		}
		if fits32 && (len(encoded32) != 4 || binary.LittleEndian.Uint32(encoded32) != v32) {
			t.Fatal("32-bit builder disagrees with its validator")
		}
	}
}

func TestSignedInformationBits(t *testing.T) {
	cases := []struct {
		value int32
		bits  uint32
	}{{-2147483648, 0x80000000}, {-1, 0xffffffff}, {0, 0}, {1, 1}, {2147483647, 0x7fffffff}}
	for _, c := range cases {
		value := FilesystemAttributeInformation{MaxComponentLength: c.value}
		data, err := EncodeFilesystemAttributeInformation(value)
		if err != nil {
			t.Fatal(err)
		}
		if binary.LittleEndian.Uint32(data[4:]) != c.bits {
			t.Fatalf("signed field %d: %x", c.value, data)
		}
		got, err := DecodeFilesystemAttributeInformation(data)
		if err != nil || got != value {
			t.Fatalf("signed field round trip: %+v, %v", got, err)
		}
	}
}

func TestSetInfoResponseLayout(t *testing.T) {
	data, err := EncodeSetInfoResponse(EmptyResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte{2, 0}) {
		t.Fatalf("SET_INFO response: %x", data)
	}
}
