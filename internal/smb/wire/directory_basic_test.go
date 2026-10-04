package wire

import (
	"encoding/binary"
	"reflect"
	"testing"
)

type basicDirectoryCodec struct {
	encode func([]DirectoryEntry) ([]byte, error)
	decode func([]byte) ([]DirectoryEntry, error)
	name   string
	fixed  int
}

func basicDirectoryCodecs() []basicDirectoryCodec {
	return []basicDirectoryCodec{
		{EncodeDirectoryEntries, DecodeDirectoryEntries, "directory", 64},
		{EncodeDirectoryFullEntries, DecodeDirectoryFullEntries, "full", 68},
		{EncodeDirectoryBothEntries, DecodeDirectoryBothEntries, "both", 94},
		{EncodeDirectoryNamesEntries, DecodeDirectoryNamesEntries, "names", 12},
	}
}

func basicDirectorySample(fixed int) []DirectoryEntry {
	metadata := DirectoryMetadata{FileIndex: 7}
	if fixed != 12 {
		metadata.Basic = FileBasicInformation{Created: 11, Accessed: 12, Modified: 13, Changed: 14, Attributes: 0x20}
		metadata.EndOfFile, metadata.AllocationSize = 101, 4096
	}
	if fixed == 68 || fixed == 94 {
		metadata.EASize = 17
	}
	entry := DirectoryEntry{Name: "a😀", Metadata: metadata}
	if fixed == 94 {
		entry.ShortName = "A"
	}
	return []DirectoryEntry{entry, {Name: "second", Metadata: metadata}}
}

func TestBasicDirectoryCodecsRoundTripAndLayout(t *testing.T) {
	for _, codec := range basicDirectoryCodecs() {
		t.Run(codec.name, func(t *testing.T) {
			entries := basicDirectorySample(codec.fixed)
			data, err := codec.encode(entries)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := codec.decode(data)
			if err != nil || !reflect.DeepEqual(decoded, entries) {
				t.Fatalf("round trip: %+v, %v", decoded, err)
			}
			checkBasicDirectoryLayout(t, codec.fixed, data)
			empty, err := codec.encode(nil)
			if err != nil || len(empty) != 0 {
				t.Fatalf("empty encode: %x, %v", empty, err)
			}
			decoded, err = codec.decode(empty)
			if err != nil || len(decoded) != 0 {
				t.Fatalf("empty decode: %+v, %v", decoded, err)
			}
		})
	}
}

func checkBasicDirectoryLayout(t *testing.T, fixed int, data []byte) {
	t.Helper()
	firstSize := fixed + 6
	next := (firstSize + 7) &^ 7
	if int(binary.LittleEndian.Uint32(data[:4])) != next || binary.LittleEndian.Uint32(data[next:next+4]) != 0 {
		t.Fatalf("links: %x", data)
	}
	lengthOffset := 60
	if fixed == 12 {
		lengthOffset = 8
	} else if binary.LittleEndian.Uint64(data[40:48]) != 101 || binary.LittleEndian.Uint64(data[48:56]) != 4096 {
		t.Fatal("incorrect length offsets")
	}
	if binary.LittleEndian.Uint32(data[lengthOffset:lengthOffset+4]) != 6 || !reflect.DeepEqual(data[fixed:firstSize], []byte{'a', 0, 0x3d, 0xd8, 0, 0xde}) {
		t.Fatal("incorrect UTF-16 name layout")
	}
	for _, b := range data[firstSize:next] {
		if b != 0 {
			t.Fatal("nonzero padding")
		}
	}
}

func TestBasicDirectoryCodecsRejectMalformedLists(t *testing.T) {
	for _, codec := range basicDirectoryCodecs() {
		t.Run(codec.name, func(t *testing.T) {
			data, err := codec.encode(basicDirectorySample(codec.fixed))
			if err != nil {
				t.Fatal(err)
			}
			for end := 1; end < codec.fixed; end++ {
				if _, err := codec.decode(data[:end]); err == nil {
					t.Fatalf("accepted truncated fixed fields at %d", end)
				}
			}
			for _, change := range []func([]byte){
				func(b []byte) { binary.LittleEndian.PutUint32(b, 7) },
				func(b []byte) { binary.LittleEndian.PutUint32(b, 0xfffffff8) },
				func(b []byte) { b[codec.fixed], b[codec.fixed+1] = 0, 0xd8 },
				func(b []byte) { b[len(b)-1] = 0xd8 },
			} {
				bad := append([]byte(nil), data...)
				change(bad)
				if _, err := codec.decode(bad); err == nil {
					t.Fatal("accepted malformed list")
				}
			}
			if _, err := codec.encode([]DirectoryEntry{{Name: string([]byte{0xff})}}); err == nil {
				t.Fatal("accepted invalid UTF-8 name")
			}
		})
	}
}

func TestDirectoryBothRejectsLongShortName(t *testing.T) {
	if _, err := EncodeDirectoryBothEntries([]DirectoryEntry{{Name: "long", ShortName: "1234567890123"}}); err == nil {
		t.Fatal("accepted overlong short name")
	}
	data, err := EncodeDirectoryBothEntries([]DirectoryEntry{{Name: "long"}})
	if err != nil {
		t.Fatal(err)
	}
	data[68] = 25
	if _, err := DecodeDirectoryBothEntries(data); err == nil {
		t.Fatal("accepted odd or overlong short-name length")
	}
}

func FuzzDecodeDirectoryEntries(f *testing.F) {
	fuzzCodec(f, basicDirectorySample(64), EncodeDirectoryEntries, DecodeDirectoryEntries)
}

func FuzzDecodeDirectoryFullEntries(f *testing.F) {
	fuzzCodec(f, basicDirectorySample(68), EncodeDirectoryFullEntries, DecodeDirectoryFullEntries)
}

func FuzzDecodeDirectoryBothEntries(f *testing.F) {
	fuzzCodec(f, basicDirectorySample(94), EncodeDirectoryBothEntries, DecodeDirectoryBothEntries)
}

func FuzzDecodeDirectoryNamesEntries(f *testing.F) {
	fuzzCodec(f, basicDirectorySample(12), EncodeDirectoryNamesEntries, DecodeDirectoryNamesEntries)
}
