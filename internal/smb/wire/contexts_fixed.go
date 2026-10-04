package wire

// EncodeFileIDQuery returns an empty QFid context.
func EncodeFileIDQuery(FileIDQuery) (CreateContext, error) {
	return CreateContext{Name: "QFid"}, nil
}

// DecodeDurableRequest checks the DH2Q tag and payload.
func DecodeDurableRequest(c CreateContext) (DurableRequest, error) {
	var v DurableRequest
	if c.Name != "DH2Q" {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	v.Timeout = r.u32()
	v.Flags = r.u32()
	r.skip(8)
	v.CreateGUID = r.guid()
	return v, r.exact()
}

// EncodeDurableRequest writes a tagged DH2Q payload.
func EncodeDurableRequest(v DurableRequest) (CreateContext, error) {
	b := builder{}
	b.u32(v.Timeout)
	b.u32(v.Flags)
	b.zero(8)
	b.guid(v.CreateGUID)
	return CreateContext{Name: "DH2Q", Data: b.data}, nil
}

// DecodeDurableReply checks the DH2Q tag and payload.
func DecodeDurableReply(c CreateContext) (DurableReply, error) {
	var v DurableReply
	if c.Name != "DH2Q" {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	v.Timeout = r.u32()
	v.Flags = r.u32()
	return v, r.exact()
}

// EncodeDurableReply writes a tagged DH2Q payload.
func EncodeDurableReply(v DurableReply) (CreateContext, error) {
	b := builder{}
	b.u32(v.Timeout)
	b.u32(v.Flags)
	return CreateContext{Name: "DH2Q", Data: b.data}, nil
}

// DecodeDurableReconnect checks the DH2C tag and payload.
func DecodeDurableReconnect(c CreateContext) (DurableReconnect, error) {
	var v DurableReconnect
	if c.Name != "DH2C" {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	v.ID = r.id()
	v.CreateGUID = r.guid()
	v.Flags = r.u32()
	return v, r.exact()
}

// EncodeDurableReconnect writes a tagged DH2C payload.
func EncodeDurableReconnect(v DurableReconnect) (CreateContext, error) {
	b := builder{}
	b.id(v.ID)
	b.guid(v.CreateGUID)
	b.u32(v.Flags)
	return CreateContext{Name: "DH2C", Data: b.data}, nil
}
