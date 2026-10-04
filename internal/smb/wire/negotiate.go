package wire

// DecodeNegotiateRequest reads dialects and the header-relative context chain.
func DecodeNegotiateRequest(m Message) (NegotiateRequest, error) {
	r := body(m, Negotiate, false, 36, 36)
	var v NegotiateRequest
	count := r.u16()
	v.SecurityMode = r.u16()
	r.skip(2)
	v.Capabilities = r.u32()
	v.ClientGUID = r.guid()
	offset := r.u32()
	contextCount := r.u16()
	r.skip(2)
	if r.err != nil || count == 0 || int(count) > (len(m.Body)-36)/2 {
		return v, errMalformed
	}
	v.Dialects = make([]uint16, count)
	has311 := false
	for i := range v.Dialects {
		v.Dialects[i] = r.u16()
		has311 = has311 || v.Dialects[i] == 0x0311
	}
	if has311 {
		v.Contexts = r.negotiateContexts(offset, contextCount, 36+uint64(count)*2)
	}
	return v, r.err
}

// EncodeNegotiateRequest writes dialect offers and aligned contexts.
func EncodeNegotiateRequest(v NegotiateRequest) ([]byte, error) {
	if len(v.Dialects) == 0 || !size16(len(v.Dialects)) {
		return nil, errMalformed
	}
	has311 := false
	for _, d := range v.Dialects {
		has311 = has311 || d == 0x0311
	}
	if !has311 && len(v.Contexts) > 0 {
		return nil, errMalformed
	}
	contexts, err := encodeNegotiateContexts(v.Contexts)
	if err != nil {
		return nil, err
	}
	offset := 36 + 2*len(v.Dialects)
	offset += (8 - offset%8) % 8
	b := builder{}
	b.u16(36)
	b.length16(len(v.Dialects))
	b.u16(v.SecurityMode)
	b.u16(0)
	b.u32(v.Capabilities)
	b.guid(v.ClientGUID)
	if len(v.Contexts) > 0 {
		b.length32(offset + 64)
	} else {
		b.u32(0)
	}
	b.length16(len(v.Contexts))
	b.u16(0)
	for _, d := range v.Dialects {
		b.u16(d)
	}
	if len(contexts) > 0 {
		b.align8()
		b.bytes(contexts)
	}
	return b.finish()
}

// DecodeNegotiateResponse reads the selected dialect, token and contexts.
func DecodeNegotiateResponse(m Message) (NegotiateResponse, error) {
	r := body(m, Negotiate, true, 65, 64)
	var v NegotiateResponse
	v.SecurityMode = r.u16()
	v.Dialect = r.u16()
	count := r.u16()
	v.ServerGUID = r.guid()
	v.Capabilities = r.u32()
	v.MaxTransact = r.u32()
	v.MaxRead = r.u32()
	v.MaxWrite = r.u32()
	v.SystemTime = r.u64()
	v.ServerStartTime = r.u64()
	tokenOffset := r.u16()
	tokenLength := r.u16()
	contextOffset := r.u32()
	v.Token = clone(r.field(uint64(tokenOffset), uint64(tokenLength), 64, 1))
	if v.Dialect == 0x0311 {
		v.Contexts = r.negotiateContexts(contextOffset, count, 64)
	}
	return v, r.err
}

// EncodeNegotiateResponse writes the selected dialect and aligned contexts.
func EncodeNegotiateResponse(v NegotiateResponse) ([]byte, error) {
	if !size16(len(v.Token)) || v.Dialect != 0x0311 && len(v.Contexts) > 0 {
		return nil, errMalformed
	}
	contexts, err := encodeNegotiateContexts(v.Contexts)
	if err != nil {
		return nil, err
	}
	offset := 64 + len(v.Token)
	offset += (8 - offset%8) % 8
	b := builder{}
	b.u16(65)
	b.u16(v.SecurityMode)
	b.u16(v.Dialect)
	b.length16(len(v.Contexts))
	b.guid(v.ServerGUID)
	b.u32(v.Capabilities)
	b.u32(v.MaxTransact)
	b.u32(v.MaxRead)
	b.u32(v.MaxWrite)
	b.u64(v.SystemTime)
	b.u64(v.ServerStartTime)
	b.u16(128)
	b.length16(len(v.Token))
	if len(contexts) > 0 {
		b.length32(offset + 64)
	} else {
		b.u32(0)
	}
	b.bytes(v.Token)
	if len(contexts) > 0 {
		b.align8()
		b.bytes(contexts)
	}
	return b.finish()
}
