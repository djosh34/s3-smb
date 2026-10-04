package wire

func decodeAlgorithms(c NegotiateContext, typ uint16) ([]uint16, error) {
	if c.Type != typ || len(c.Data) > 65535 {
		return nil, errMalformed
	}
	r := reader{data: c.Data}
	count := r.u16()
	if count == 0 || int(count) > (len(c.Data)-r.pos)/2 {
		return nil, errMalformed
	}
	values := make([]uint16, count)
	for i := range values {
		values[i] = r.u16()
	}
	return values, r.exact()
}

func encodeAlgorithms(values []uint16, typ uint16) (NegotiateContext, error) {
	if len(values) == 0 || len(values) > 32766 {
		return NegotiateContext{}, errMalformed
	}
	b := builder{}
	b.length16(len(values))
	for _, v := range values {
		b.u16(v)
	}
	data, err := b.finish()
	if err != nil {
		return NegotiateContext{}, err
	}
	return NegotiateContext{Type: typ, Data: data}, nil
}

// DecodePreauthContext reads the hash list and salt.
func DecodePreauthContext(c NegotiateContext) (PreauthContext, error) {
	var v PreauthContext
	if c.Type != ContextPreauth || len(c.Data) > 65535 {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	count := r.u16()
	saltLength := r.u16()
	if r.err != nil || count == 0 || int(count) > (len(c.Data)-r.pos)/2 {
		return v, errMalformed
	}
	v.Hashes = make([]uint16, count)
	for i := range v.Hashes {
		v.Hashes[i] = r.u16()
	}
	v.Salt = clone(r.take(int(saltLength)))
	return v, r.exact()
}

// EncodePreauthContext writes a preauth hash list and salt.
func EncodePreauthContext(v PreauthContext) (NegotiateContext, error) {
	if len(v.Hashes) == 0 || 4+2*len(v.Hashes)+len(v.Salt) > 65535 {
		return NegotiateContext{}, errMalformed
	}
	b := builder{}
	b.length16(len(v.Hashes))
	b.length16(len(v.Salt))
	for _, h := range v.Hashes {
		b.u16(h)
	}
	b.bytes(v.Salt)
	data, err := b.finish()
	if err != nil {
		return NegotiateContext{}, err
	}
	return NegotiateContext{Type: ContextPreauth, Data: data}, nil
}

// DecodeEncryptionContext reads offered or selected ciphers.
func DecodeEncryptionContext(c NegotiateContext) (EncryptionContext, error) {
	values, err := decodeAlgorithms(c, ContextEncryption)
	return EncryptionContext{Ciphers: values}, err
}

// EncodeEncryptionContext writes the cipher list.
func EncodeEncryptionContext(v EncryptionContext) (NegotiateContext, error) {
	return encodeAlgorithms(v.Ciphers, ContextEncryption)
}

// DecodeSigningContext reads offered or selected algorithms.
func DecodeSigningContext(c NegotiateContext) (SigningContext, error) {
	values, err := decodeAlgorithms(c, ContextSigning)
	return SigningContext{Algorithms: values}, err
}

// EncodeSigningContext writes the signing algorithm list.
func EncodeSigningContext(v SigningContext) (NegotiateContext, error) {
	return encodeAlgorithms(v.Algorithms, ContextSigning)
}

// DecodeAAPLQuery reads the server-query request.
func DecodeAAPLQuery(c CreateContext) (AAPLQuery, error) {
	var v AAPLQuery
	if c.Name != "AAPL" {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	if r.u32() != 1 {
		return v, errMalformed
	}
	r.skip(4)
	v.Requested = r.u64()
	v.ClientCapabilities = r.u64()
	return v, r.exact()
}

// EncodeAAPLQuery writes an AAPL server query.
func EncodeAAPLQuery(v AAPLQuery) (CreateContext, error) {
	b := builder{}
	b.u32(1)
	b.u32(0)
	b.u64(v.Requested)
	b.u64(v.ClientCapabilities)
	return CreateContext{Name: "AAPL", Data: b.data}, nil
}

// DecodeAAPLReply reads the fields selected by the returned bitmap.
func DecodeAAPLReply(c CreateContext) (AAPLReply, error) {
	var v AAPLReply
	if c.Name != "AAPL" {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	if r.u32() != 1 {
		return v, errMalformed
	}
	r.skip(4)
	v.Returned = r.u64()
	if v.Returned & ^uint64(7) != 0 {
		return v, errMalformed
	}
	if v.Returned&1 != 0 {
		v.ServerCapabilities = r.u64()
	}
	if v.Returned&2 != 0 {
		v.VolumeCapabilities = r.u64()
	}
	if v.Returned&4 != 0 {
		r.skip(4)
		n := r.u32()
		if uint64(n) > uint64(len(c.Data)) {
			return v, errMalformed
		}
		v.Model = r.text(r.take(int(n)))
	}
	return v, r.exact()
}

// EncodeAAPLReply writes only bitmap-selected fields.
func EncodeAAPLReply(v AAPLReply) (CreateContext, error) {
	if v.Returned & ^uint64(7) != 0 || v.Returned&1 == 0 && v.ServerCapabilities != 0 || v.Returned&2 == 0 && v.VolumeCapabilities != 0 || v.Returned&4 == 0 && v.Model != "" {
		return CreateContext{}, errMalformed
	}
	model, err := encodeUTF16(v.Model)
	if err != nil {
		return CreateContext{}, err
	}
	b := builder{}
	b.u32(1)
	b.u32(0)
	b.u64(v.Returned)
	if v.Returned&1 != 0 {
		b.u64(v.ServerCapabilities)
	}
	if v.Returned&2 != 0 {
		b.u64(v.VolumeCapabilities)
	}
	if v.Returned&4 != 0 {
		b.u32(0)
		b.length32(len(model))
		b.bytes(model)
	}
	data, err := b.finish()
	if err != nil {
		return CreateContext{}, err
	}
	return CreateContext{Name: "AAPL", Data: data}, nil
}

// DecodeLeaseContext infers V1 or V2 from the exact payload length.
func DecodeLeaseContext(c CreateContext) (LeaseContext, error) {
	var v LeaseContext
	if c.Name != "RqLs" || len(c.Data) != 32 && len(c.Data) != 52 {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	v.Key = r.guid()
	v.State = r.u32()
	v.Flags = r.u32()
	v.Duration = r.u64()
	v.Version = 1
	if len(c.Data) == 52 {
		v.Version = 2
		v.ParentKey = r.guid()
		v.Epoch = r.u16()
		r.skip(2)
	}
	return v, r.exact()
}

// EncodeLeaseContext writes the requested lease version.
func EncodeLeaseContext(v LeaseContext) (CreateContext, error) {
	if v.Version != 1 && v.Version != 2 || v.Version == 1 && (v.ParentKey != [16]byte{} || v.Epoch != 0) {
		return CreateContext{}, errMalformed
	}
	b := builder{}
	b.guid(v.Key)
	b.u32(v.State)
	b.u32(v.Flags)
	b.u64(v.Duration)
	if v.Version == 2 {
		b.guid(v.ParentKey)
		b.u16(v.Epoch)
		b.u16(0)
	}
	return CreateContext{Name: "RqLs", Data: b.data}, nil
}
