package wire

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

var errMalformed = errors.New("wire: malformed encoding")

// reader records the first error. All reads after a short read remain safe.
type reader struct {
	err    error
	data   []byte
	ranges []span
	pos    int
}

type span struct{ start, end uint64 }

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.data)-r.pos {
		r.err = errMalformed
		return nil
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b
}
func (r *reader) skip(n int) { r.take(n) }
func (r *reader) u8() uint8 {
	b := r.take(1)
	if len(b) != 1 {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if len(b) != 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if len(b) != 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if len(b) != 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}
func (r *reader) guid() [16]byte { var v [16]byte; copy(v[:], r.take(16)); return v }
func (r *reader) id() FileID     { return FileID{Persistent: r.u64(), Volatile: r.u64()} }
func (r *reader) boolean() bool {
	return r.u8() != 0
}

func (r *reader) exact() error {
	if r.pos != len(r.data) {
		r.err = errMalformed
	}
	return r.err
}

// A terminal list member can end with at most one alignment unit's padding.
func (r *reader) paddingAfter(end uint64) {
	if end > uint64(len(r.data)) || uint64(len(r.data))-end > 7 {
		r.err = errMalformed
	}
}

// region validates variable fields against the fixed prefix and each other.
// Offsets are local to data; command callers subtract the SMB header separately.
func (r *reader) region(offset, length, minimum, alignment uint64) []byte {
	if r.err != nil {
		return nil
	}
	if length == 0 {
		if offset != 0 && (offset < minimum || offset > uint64(len(r.data)) || offset%alignment != 0) {
			r.err = errMalformed
		}
		return nil
	}
	if offset < minimum || offset%alignment != 0 || offset > uint64(len(r.data)) || length > uint64(len(r.data))-offset {
		r.err = errMalformed
		return nil
	}
	end := offset + length
	for _, s := range r.ranges {
		if offset < s.end && s.start < end {
			r.err = errMalformed
			return nil
		}
	}
	r.ranges = append(r.ranges, span{offset, end})
	return r.data[offset:end]
}

func (r *reader) field(offset, length uint64, minimum uint64, alignment uint64) []byte {
	if length == 0 && offset == 0 {
		return nil
	}
	if offset < 64 {
		r.err = errMalformed
		return nil
	}
	return r.region(offset-64, length, minimum, alignment)
}

func (r *reader) text(b []byte) string {
	s, err := decodeUTF16(b)
	if err != nil {
		r.err = err
	}
	return s
}

func clone(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

func decodeUTF16(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", errMalformed
	}
	words := make([]uint16, len(b)/2)
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	for i := 0; i < len(words); i++ {
		v := words[i]
		if v >= 0xd800 && v <= 0xdbff {
			i++
			if i >= len(words) || words[i] < 0xdc00 || words[i] > 0xdfff {
				return "", errMalformed
			}
		} else if v >= 0xdc00 && v <= 0xdfff {
			return "", errMalformed
		}
	}
	return string(utf16.Decode(words)), nil
}

func encodeUTF16(s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, errMalformed
	}
	w := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(w)*2)
	for _, v := range w {
		b = binary.LittleEndian.AppendUint16(b, v)
	}
	return b, nil
}

// builder emits fields in wire order. Padding is always zero.
type builder struct {
	err  error
	data []byte
}

func (b *builder) u8(v uint8)      { b.data = append(b.data, v) }
func (b *builder) u16(v uint16)    { b.data = binary.LittleEndian.AppendUint16(b.data, v) }
func (b *builder) u32(v uint32)    { b.data = binary.LittleEndian.AppendUint32(b.data, v) }
func (b *builder) u64(v uint64)    { b.data = binary.LittleEndian.AppendUint64(b.data, v) }
func (b *builder) bytes(v []byte)  { b.data = append(b.data, v...) }
func (b *builder) zero(n int)      { b.data = append(b.data, make([]byte, n)...) }
func (b *builder) guid(v [16]byte) { b.bytes(v[:]) }
func (b *builder) id(v FileID)     { b.u64(v.Persistent); b.u64(v.Volatile) }
func (b *builder) boolean(v bool) {
	if v {
		b.u8(1)
	} else {
		b.u8(0)
	}
}

// align8 pads with zeros to the next eight-byte boundary.
func (b *builder) align8() { b.zero((8 - len(b.data)%8) % 8) }

// Length conversions check the field width before narrowing.
func (b *builder) length8(n int) {
	if n < 0 || n > 255 {
		b.err = errMalformed
		return
	}
	b.u8(uint8(n))
}

func (b *builder) length16(n int) {
	v, err := count16(n)
	if err != nil {
		b.err = err
		return
	}
	b.u16(v)
}

func count16(n int) (uint16, error) {
	if n < 0 || n > 65535 {
		return 0, errMalformed
	}
	return uint16(n), nil
}

func (b *builder) length32(n int) {
	v, err := count32(n)
	if err != nil {
		b.err = err
		return
	}
	b.u32(v)
}

func count32(n int) (uint32, error) {
	if n < 0 || uint64(n) > 4294967295 {
		return 0, errMalformed
	}
	return uint32(n), nil
}

func (b *builder) encoded(data []byte, err error) {
	if b.err != nil {
		return
	}
	if err != nil {
		b.err = err
		return
	}
	b.bytes(data)
}

func (b *builder) finish() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.data, nil
}

// The masks preserve two's-complement bits and keep gosec's casts in range.
func (r *reader) i32() int32 {
	v := r.u32()
	if v&0x80000000 != 0 {
		return -1 - int32(^v&0x7fffffff)
	}
	return int32(v & 0x7fffffff)
}

func (b *builder) i32(v int32) {
	if v >= 0 {
		b.u32(uint32(v))
		return
	}
	b.u32(^uint32(-(v + 1) & 0x7fffffff))
}
func size16(n int) bool { _, err := count16(n); return err == nil }
func size32(n int) bool { _, err := count32(n); return err == nil }

func body(m Message, command Command, response bool, size uint16, fixed int) *reader {
	r := &reader{data: m.Body}
	if m.Header.Command != command || (m.Header.Flags&FlagResponse != 0) != response || len(m.Body) < fixed || r.u16() != size {
		r.err = errMalformed
	}
	return r
}
