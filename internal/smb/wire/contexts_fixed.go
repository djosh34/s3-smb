package wire

import "github.com/djosh34/s3-smb/internal/smb"

// DecodeMaxAccessQuery checks the MxAc tag and payload.
func DecodeMaxAccessQuery(c CreateContext) (MaxAccessQuery, error) {
	var v MaxAccessQuery
	if c.Name != "MxAc" {
		return v, errMalformed
	}
	r := reader{data: c.Data}
	if len(c.Data) == 0 {
		return v, nil
	}
	v.Timestamp = r.u64()
	return v, r.exact()
}

// EncodeMaxAccessQuery writes a tagged MxAc payload.
func EncodeMaxAccessQuery(v MaxAccessQuery) (CreateContext, error) {
	b := builder{}
	b.u64(v.Timestamp)
	return CreateContext{Name: "MxAc", Data: b.data}, nil
}

// DecodeMaxAccessReply checks the MxAc tag and payload.
func DecodeMaxAccessReply(c CreateContext) (MaxAccessReply, error) {
	var v MaxAccessReply
	if c.Name != "MxAc" {
		return v, errMalformed
	}
	r := reader{data: c.Data}

	v.Status = smb.Status(r.u32())
	v.Access = r.u32()
	return v, r.exact()
}

// EncodeMaxAccessReply writes a tagged MxAc payload.
func EncodeMaxAccessReply(v MaxAccessReply) (CreateContext, error) {
	b := builder{}
	b.u32(uint32(v.Status))
	b.u32(v.Access)
	return CreateContext{Name: "MxAc", Data: b.data}, nil
}

// DecodeFileIDQuery checks the QFid tag and payload.
func DecodeFileIDQuery(c CreateContext) (FileIDQuery, error) {
	var v FileIDQuery
	if c.Name != "QFid" {
		return v, errMalformed
	}
	r := reader{data: c.Data}

	return v, r.exact()
}

// EncodeFileIDQuery writes a tagged QFid payload.
func EncodeFileIDQuery(_ FileIDQuery) (CreateContext, error) {
	b := builder{}

	return CreateContext{Name: "QFid", Data: b.data}, nil
}

// DecodeFileIDReply checks the QFid tag and payload.
func DecodeFileIDReply(c CreateContext) (FileIDReply, error) {
	var v FileIDReply
	if c.Name != "QFid" {
		return v, errMalformed
	}
	r := reader{data: c.Data}

	v.DiskFileID = r.u64()
	v.VolumeID = r.u64()
	r.skip(16)
	return v, r.exact()
}

// EncodeFileIDReply writes a tagged QFid payload.
func EncodeFileIDReply(v FileIDReply) (CreateContext, error) {
	b := builder{}
	b.u64(v.DiskFileID)
	b.u64(v.VolumeID)
	b.zero(16)
	return CreateContext{Name: "QFid", Data: b.data}, nil
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
