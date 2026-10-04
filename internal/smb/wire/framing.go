package wire

import (
	"bytes"

	"github.com/djosh34/s3-smb/internal/smb"
)

// DecodeHeader reads the synchronous or asynchronous SMB2 header.
func DecodeHeader(packet []byte) (Header, error) {
	r := reader{data: packet}
	var h Header
	if !bytes.Equal(r.take(4), []byte{0xfe, 'S', 'M', 'B'}) || r.u16() != 64 {
		return h, errMalformed
	}
	h.CreditCharge = r.u16()
	status := r.u32()
	h.Command = Command(r.u16())
	h.Credit = r.u16()
	h.Flags = HeaderFlags(r.u32())
	h.NextCommand = r.u32()
	h.MessageID = r.u64()
	if h.Flags&FlagResponse != 0 {
		h.Status = smb.Status(status)
	} else {
		// The upper word is reserved in requests and must be ignored.
		h.ChannelSequence = uint16(status & 0xffff)
	}
	if h.Flags&FlagAsync != 0 {
		h.AsyncID = r.u64()
	} else {
		h.ProcessID = r.u32()
		h.TreeID = r.u32()
	}
	h.SessionID = r.u64()
	h.Signature = r.guid()
	if r.err != nil || h.NextCommand != 0 && (h.NextCommand < 64 || h.NextCommand%8 != 0) {
		return Header{}, errMalformed
	}
	return h, nil
}

// EncodeHeader writes exactly 64 bytes, rejecting fields from the other layout.
func EncodeHeader(h Header) ([]byte, error) {
	if h.Flags&FlagResponse != 0 && h.ChannelSequence != 0 || h.Flags&FlagResponse == 0 && h.Status != 0 || h.Flags&FlagAsync != 0 && (h.ProcessID != 0 || h.TreeID != 0) || h.Flags&FlagAsync == 0 && h.AsyncID != 0 || h.NextCommand != 0 && (h.NextCommand < 64 || h.NextCommand%8 != 0) {
		return nil, errMalformed
	}
	b := builder{}
	b.bytes([]byte{0xfe, 'S', 'M', 'B'})
	b.u16(64)
	b.u16(h.CreditCharge)
	if h.Flags&FlagResponse != 0 {
		b.u32(uint32(h.Status))
	} else {
		b.u16(h.ChannelSequence)
		b.u16(0)
	}
	b.u16(uint16(h.Command))
	b.u16(h.Credit)
	b.u32(uint32(h.Flags))
	b.u32(h.NextCommand)
	b.u64(h.MessageID)
	if h.Flags&FlagAsync != 0 {
		b.u64(h.AsyncID)
	} else {
		b.u32(h.ProcessID)
		b.u32(h.TreeID)
	}
	b.u64(h.SessionID)
	b.guid(h.Signature)
	return b.data, nil
}

// Split validates every link before returning any independently owned member.
func Split(packet []byte) ([]Message, error) {
	var messages []Message
	for len(packet) > 0 {
		h, err := DecodeHeader(packet)
		if err != nil {
			return nil, err
		}
		n := len(packet)
		if h.NextCommand != 0 {
			if uint64(h.NextCommand) > uint64(n) || uint64(h.NextCommand)+64 > uint64(n) {
				return nil, errMalformed
			}
			n = int(h.NextCommand)
		}
		raw := clone(packet[:n])
		messages = append(messages, Message{Header: h, Body: raw[64:], Raw: raw})
		packet = packet[n:]
	}
	if len(messages) == 0 {
		return nil, errMalformed
	}
	return messages, nil
}

// Join pads compound members and replaces supplied NextCommand links.
func Join(messages []Message) ([]byte, error) {
	if len(messages) == 0 {
		return nil, errMalformed
	}
	b := builder{}
	for i, m := range messages {
		h := m.Header
		h.NextCommand = 0
		padding := 0
		if i < len(messages)-1 {
			padding = (8 - len(m.Body)%8) % 8
			var err error
			h.NextCommand, err = count32(len(m.Body) + 64 + padding)
			if err != nil {
				return nil, err
			}
		}
		header, err := EncodeHeader(h)
		if err != nil {
			return nil, err
		}
		b.bytes(header)
		b.bytes(m.Body)
		b.zero(padding)
	}
	return b.data, nil
}

// DecodeSMB1Negotiate accepts only the opening SMB1 request offering SMB2.
func DecodeSMB1Negotiate(packet []byte) error {
	r := reader{data: packet}
	if !bytes.Equal(r.take(4), []byte{0xff, 'S', 'M', 'B'}) || r.u8() != 0x72 {
		return errMalformed
	}
	if r.u32() != 0 || r.u8()&0x80 != 0 {
		return errMalformed
	}
	r.skip(22)
	if r.u8() != 0 {
		return errMalformed
	}
	n := int(r.u16())
	dialects := r.take(n)
	if err := r.exact(); err != nil {
		return err
	}
	offered := false
	for len(dialects) > 0 {
		if dialects[0] != 2 {
			return errMalformed
		}
		dialects = dialects[1:]
		end := bytes.IndexByte(dialects, 0)
		if end < 0 {
			return errMalformed
		}
		name := string(dialects[:end])
		if name == "SMB 2.002" || name == "SMB 2.???" {
			offered = true
		}
		dialects = dialects[end+1:]
	}
	if !offered {
		return errMalformed
	}
	return nil
}
