package wire

// OplockBreakRequest is a classic oplock acknowledgment, not a lease break.
// The server grants no classic oplocks but still decodes this request.
type OplockBreakRequest struct {
	ID    FileID
	Level uint8
}

// DecodeOplockBreakRequest reads the 24-byte acknowledgment (MS-SMB2 2.2.24.1).
func DecodeOplockBreakRequest(message Message) (OplockBreakRequest, error) {
	r := body(message, OplockBreak, false, 24, 24)
	var request OplockBreakRequest
	request.Level = r.u8()
	r.skip(5)
	request.ID = r.id()
	return request, r.err
}

// EncodeOplockBreakRequest writes a classic oplock acknowledgment.
func EncodeOplockBreakRequest(request OplockBreakRequest) ([]byte, error) {
	b := builder{}
	b.u16(24)
	b.u8(request.Level)
	b.zero(5)
	b.id(request.ID)
	return b.data, nil
}
