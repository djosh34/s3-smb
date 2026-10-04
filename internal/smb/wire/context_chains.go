package wire

import "encoding/binary"

func decodeCreateContexts(data []byte) ([]CreateContext, error) {
	var contexts []CreateContext
	for len(data) > 0 {
		r := reader{data: data}
		next := r.u32()
		nameOffset := r.u16()
		nameLength := r.u16()
		r.skip(2)
		dataOffset := r.u16()
		dataLength := r.u32()
		if r.err != nil {
			return nil, r.err
		}
		if next != 0 {
			if next < 16 || next%8 != 0 || uint64(next)+16 > uint64(len(data)) {
				return nil, errMalformed
			}
			r.data = data[:int(next)]
		}
		name := r.region(uint64(nameOffset), uint64(nameLength), 16, 8)
		var payload []byte
		if dataLength != 0 {
			payload = r.region(uint64(dataOffset), uint64(dataLength), 16, 8)
		}
		if next == 0 {
			end := uint64(16)
			for _, field := range r.ranges {
				if field.end > end {
					end = field.end
				}
			}
			r.paddingAfter(end)
		}
		if r.err != nil || len(name) == 0 {
			return nil, errMalformed
		}
		contexts = append(contexts, CreateContext{Name: string(name), Data: clone(payload)})
		if next == 0 {
			break
		}
		data = data[int(next):]
	}
	return contexts, nil
}

func encodeCreateContexts(contexts []CreateContext) ([]byte, error) {
	b := builder{}
	for i, c := range contexts {
		if len(c.Name) == 0 || !size16(len(c.Name)) || !size32(len(c.Data)) {
			return nil, errMalformed
		}
		dataOffset := 16 + len(c.Name)
		dataOffset += (8 - dataOffset%8) % 8
		if !size16(dataOffset) || !size32(dataOffset+len(c.Data)+7) {
			return nil, errMalformed
		}
		member := builder{}
		member.u32(0)
		member.u16(16)
		member.length16(len(c.Name))
		member.u16(0)
		if len(c.Data) == 0 {
			member.u16(0)
		} else {
			member.length16(dataOffset)
		}
		member.length32(len(c.Data))
		member.bytes([]byte(c.Name))
		member.align8()
		member.bytes(c.Data)
		if i < len(contexts)-1 {
			member.align8()
			length, err := count32(len(member.data))
			if err != nil {
				return nil, err
			}
			binary.LittleEndian.PutUint32(member.data, length)
		}
		data, err := member.finish()
		if err != nil {
			return nil, err
		}
		b.bytes(data)
	}
	return b.finish()
}

func decodeNegotiateContexts(data []byte, count uint16) ([]NegotiateContext, error) {
	var contexts []NegotiateContext
	r := reader{data: data}
	for i := uint16(0); i < count; i++ {
		typ := r.u16()
		n := int(r.u16())
		r.skip(4)
		payload := r.take(n)
		if r.err != nil {
			return nil, r.err
		}
		contexts = append(contexts, NegotiateContext{Type: typ, Data: clone(payload)})
		if i+1 < count {
			r.skip((8 - r.pos%8) % 8)
		}
	}
	if len(r.data)-r.pos > 7 {
		return nil, errMalformed
	}
	return contexts, r.err
}

func encodeNegotiateContexts(contexts []NegotiateContext) ([]byte, error) {
	if !size16(len(contexts)) {
		return nil, errMalformed
	}
	b := builder{}
	for i, c := range contexts {
		if !size16(len(c.Data)) {
			return nil, errMalformed
		}
		if i > 0 {
			b.align8()
		}
		b.u16(c.Type)
		b.length16(len(c.Data))
		b.u32(0)
		b.bytes(c.Data)
	}
	return b.finish()
}

func (r *reader) createContexts(offset, length uint32, minimum uint64) []CreateContext {
	data := r.field(uint64(offset), uint64(length), minimum, 8)
	if r.err != nil {
		return nil
	}
	contexts, err := decodeCreateContexts(data)
	if err != nil {
		r.err = err
	}
	return contexts
}

func (r *reader) negotiateContexts(offset uint32, count uint16, minimum uint64) []NegotiateContext {
	if count == 0 {
		if offset != 0 {
			r.err = errMalformed
		}
		return nil
	}
	if uint64(offset) < minimum+64 || uint64(offset) > uint64(len(r.data))+64 || offset%8 != 0 {
		r.err = errMalformed
		return nil
	}
	start := uint64(offset) - 64
	// A context chain owns the tail of the command body, including padding.
	data := r.region(start, uint64(len(r.data))-start, minimum, 8)
	if r.err != nil {
		return nil
	}
	contexts, err := decodeNegotiateContexts(data, count)
	if err != nil {
		r.err = err
	}
	return contexts
}
