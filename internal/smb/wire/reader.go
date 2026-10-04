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
	data   []byte
	pos    int
	err    error
	ranges []span
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
	b := r.u8()
	if b > 1 {
		r.err = errMalformed
	}
	return b == 1
}
func (r *reader) exact() error {
	if r.pos != len(r.data) {
		r.err = errMalformed
	}
	return r.err
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
	return r.data[int(offset):int(end)]
}
func (r *reader) field(offset, length uint64, minimum int, alignment uint64) []byte {
	if length == 0 && offset == 0 {
		return nil
	}
	if offset < 64 {
		r.err = errMalformed
		return nil
	}
	return r.region(offset-64, length, uint64(minimum), alignment)
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
type builder struct{ data []byte }

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
func (b *builder) align(n int) { b.zero((n - len(b.data)%n) % n) }
func size16(n int) bool        { return n >= 0 && uint64(n) <= 0xffff }
func size32(n int) bool        { return n >= 0 && uint64(n) <= 0xffffffff }

func body(m Message, command Command, response bool, size uint16, fixed int) *reader {
	r := &reader{data: m.Body}
	if m.Header.Command != command || (m.Header.Flags&FlagResponse != 0) != response || len(m.Body) < fixed || r.u16() != size {
		r.err = errMalformed
	}
	return r
}
