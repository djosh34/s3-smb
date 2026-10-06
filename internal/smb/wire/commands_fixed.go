package wire

// DecodeTreeConnectResponse validates and reads the TreeConnect body.
func DecodeTreeConnectResponse(m Message) (TreeConnectResponse, error) {
	r := body(m, TreeConnect, true, 16, 16)
	var v TreeConnectResponse
	v.ShareType = r.u8()
	r.skip(1)
	v.Flags = r.u32()
	v.Capabilities = r.u32()
	v.MaximalAccess = r.u32()
	return v, r.err
}

// EncodeTreeConnectResponse writes the TreeConnect body.
func EncodeTreeConnectResponse(v TreeConnectResponse) ([]byte, error) {
	b := builder{}
	b.u16(16)
	b.u8(v.ShareType)
	b.zero(1)
	b.u32(v.Flags)
	b.u32(v.Capabilities)
	b.u32(v.MaximalAccess)
	return b.data, nil
}

// DecodeCloseRequest validates and reads the Close body.
func DecodeCloseRequest(m Message) (CloseRequest, error) {
	r := body(m, Close, false, 24, 24)
	var v CloseRequest
	v.Flags = r.u16()
	r.skip(4)
	v.ID = r.id()
	return v, r.err
}

// EncodeCloseRequest writes the Close body.
func EncodeCloseRequest(v CloseRequest) ([]byte, error) {
	b := builder{}
	b.u16(24)
	b.u16(v.Flags)
	b.zero(4)
	b.id(v.ID)
	return b.data, nil
}

// DecodeCloseResponse validates and reads the Close body.
func DecodeCloseResponse(m Message) (CloseResponse, error) {
	r := body(m, Close, true, 60, 60)
	var v CloseResponse
	v.Flags = r.u16()
	r.skip(4)
	v.Created = r.u64()
	v.Accessed = r.u64()
	v.Modified = r.u64()
	v.Changed = r.u64()
	v.AllocationSize = r.u64()
	v.Size = r.u64()
	v.Attributes = r.u32()
	return v, r.err
}

// EncodeCloseResponse writes the Close body.
func EncodeCloseResponse(v CloseResponse) ([]byte, error) {
	b := builder{}
	b.u16(60)
	b.u16(v.Flags)
	b.zero(4)
	b.u64(v.Created)
	b.u64(v.Accessed)
	b.u64(v.Modified)
	b.u64(v.Changed)
	b.u64(v.AllocationSize)
	b.u64(v.Size)
	b.u32(v.Attributes)
	return b.data, nil
}

// DecodeFlushRequest validates and reads the Flush body.
func DecodeFlushRequest(m Message) (FlushRequest, error) {
	r := body(m, Flush, false, 24, 24)
	var v FlushRequest
	v.Reserved1 = r.u16()
	r.skip(4)
	v.ID = r.id()
	return v, r.err
}

// EncodeFlushRequest writes the Flush body.
func EncodeFlushRequest(v FlushRequest) ([]byte, error) {
	b := builder{}
	b.u16(24)
	b.u16(v.Reserved1)
	b.zero(4)
	b.id(v.ID)
	return b.data, nil
}

// DecodeWriteResponse validates and reads the Write body.
func DecodeWriteResponse(m Message) (WriteResponse, error) {
	r := body(m, Write, true, 17, 16)
	var v WriteResponse
	r.skip(2)
	v.Count = r.u32()
	v.Remaining = r.u32()
	r.skip(4)
	return v, r.err
}

// EncodeWriteResponse writes the Write body.
func EncodeWriteResponse(v WriteResponse) ([]byte, error) {
	b := builder{}
	b.u16(17)
	b.zero(2)
	b.u32(v.Count)
	b.u32(v.Remaining)
	b.zero(4)
	return b.data, nil
}

// DecodeChangeNotifyRequest validates and reads the ChangeNotify body.
func DecodeChangeNotifyRequest(m Message) (ChangeNotifyRequest, error) {
	r := body(m, ChangeNotify, false, 32, 32)
	var v ChangeNotifyRequest
	v.Flags = r.u16()
	v.OutputLength = r.u32()
	v.ID = r.id()
	v.Filter = r.u32()
	r.skip(4)
	return v, r.err
}

// EncodeChangeNotifyRequest writes the ChangeNotify body.
func EncodeChangeNotifyRequest(v ChangeNotifyRequest) ([]byte, error) {
	b := builder{}
	b.u16(32)
	b.u16(v.Flags)
	b.u32(v.OutputLength)
	b.id(v.ID)
	b.u32(v.Filter)
	b.zero(4)
	return b.data, nil
}

// DecodeLeaseBreakRequest validates and reads the OplockBreak body.
func DecodeLeaseBreakRequest(m Message) (LeaseBreakRequest, error) {
	r := body(m, OplockBreak, false, 36, 36)
	var v LeaseBreakRequest
	r.skip(2)
	v.Flags = r.u32()
	v.Key = r.guid()
	v.State = r.u32()
	v.Duration = r.u64()
	return v, r.err
}

// EncodeLeaseBreakRequest writes the OplockBreak body.
func EncodeLeaseBreakRequest(v LeaseBreakRequest) ([]byte, error) {
	b := builder{}
	b.u16(36)
	b.zero(2)
	b.u32(v.Flags)
	b.guid(v.Key)
	b.u32(v.State)
	b.u64(v.Duration)
	return b.data, nil
}

// DecodeLeaseBreakResponse validates and reads the OplockBreak body.
func DecodeLeaseBreakResponse(m Message) (LeaseBreakResponse, error) {
	r := body(m, OplockBreak, true, 36, 36)
	var v LeaseBreakResponse
	r.skip(2)
	v.Flags = r.u32()
	v.Key = r.guid()
	v.State = r.u32()
	v.Duration = r.u64()
	return v, r.err
}

// EncodeLeaseBreakResponse writes the OplockBreak body.
func EncodeLeaseBreakResponse(v LeaseBreakResponse) ([]byte, error) {
	b := builder{}
	b.u16(36)
	b.zero(2)
	b.u32(v.Flags)
	b.guid(v.Key)
	b.u32(v.State)
	b.u64(v.Duration)
	return b.data, nil
}

// DecodeLeaseBreakNotification validates and reads the OplockBreak body.
func DecodeLeaseBreakNotification(m Message) (LeaseBreakNotification, error) {
	r := body(m, OplockBreak, true, 44, 44)
	var v LeaseBreakNotification
	v.Epoch = r.u16()
	v.Flags = r.u32()
	v.Key = r.guid()
	v.CurrentState = r.u32()
	v.NewState = r.u32()
	v.BreakReason = r.u32()
	v.AccessMaskHint = r.u32()
	v.ShareMaskHint = r.u32()
	return v, r.err
}

// EncodeLeaseBreakNotification writes the OplockBreak body.
func EncodeLeaseBreakNotification(v LeaseBreakNotification) ([]byte, error) {
	b := builder{}
	b.u16(44)
	b.u16(v.Epoch)
	b.u32(v.Flags)
	b.guid(v.Key)
	b.u32(v.CurrentState)
	b.u32(v.NewState)
	b.u32(v.BreakReason)
	b.u32(v.AccessMaskHint)
	b.u32(v.ShareMaskHint)
	return b.data, nil
}

// Empty bodies hold only the structure size and two reserved bytes, except
// SET_INFO's response, which has no reserved bytes.
func decodeEmpty(m Message, command Command, response bool) error {
	size := uint16(4)
	if command == SetInfo && response {
		size = 2
	}
	return body(m, command, response, size, int(size)).err
}

func emptyBody() []byte { return []byte{4, 0, 0, 0} }

// DecodeEchoRequest checks the command and structure size.
func DecodeEchoRequest(m Message) (EmptyRequest, error) {
	return EmptyRequest{}, decodeEmpty(m, Echo, false)
}

// EncodeEchoRequest writes the empty body.
func EncodeEchoRequest(EmptyRequest) ([]byte, error) { return emptyBody(), nil }

// DecodeEchoResponse checks the command and structure size.
func DecodeEchoResponse(m Message) (EmptyResponse, error) {
	return EmptyResponse{}, decodeEmpty(m, Echo, true)
}

// EncodeEchoResponse writes the empty body.
func EncodeEchoResponse(EmptyResponse) ([]byte, error) { return emptyBody(), nil }

// DecodeLogoffRequest checks the command and structure size.
func DecodeLogoffRequest(m Message) (EmptyRequest, error) {
	return EmptyRequest{}, decodeEmpty(m, Logoff, false)
}

// EncodeLogoffRequest writes the empty body.
func EncodeLogoffRequest(EmptyRequest) ([]byte, error) { return emptyBody(), nil }

// EncodeLogoffResponse writes the empty body.
func EncodeLogoffResponse(EmptyResponse) ([]byte, error) { return emptyBody(), nil }

// DecodeTreeDisconnectRequest checks the command and structure size.
func DecodeTreeDisconnectRequest(m Message) (EmptyRequest, error) {
	return EmptyRequest{}, decodeEmpty(m, TreeDisconnect, false)
}

// EncodeTreeDisconnectRequest writes the empty body.
func EncodeTreeDisconnectRequest(EmptyRequest) ([]byte, error) { return emptyBody(), nil }

// EncodeTreeDisconnectResponse writes the empty body.
func EncodeTreeDisconnectResponse(EmptyResponse) ([]byte, error) { return emptyBody(), nil }

// DecodeCancelRequest checks the command and structure size.
func DecodeCancelRequest(m Message) (EmptyRequest, error) {
	return EmptyRequest{}, decodeEmpty(m, Cancel, false)
}

// EncodeCancelRequest writes the empty body.
func EncodeCancelRequest(EmptyRequest) ([]byte, error) { return emptyBody(), nil }

// DecodeFlushResponse checks the command and structure size.
func DecodeFlushResponse(m Message) (EmptyResponse, error) {
	return EmptyResponse{}, decodeEmpty(m, Flush, true)
}

// EncodeFlushResponse writes the empty body.
func EncodeFlushResponse(EmptyResponse) ([]byte, error) { return emptyBody(), nil }

// DecodeSetInfoResponse checks the command and structure size.
func DecodeSetInfoResponse(m Message) (EmptyResponse, error) {
	return EmptyResponse{}, decodeEmpty(m, SetInfo, true)
}

// EncodeSetInfoResponse writes the two-byte body.
func EncodeSetInfoResponse(EmptyResponse) ([]byte, error) { return []byte{2, 0}, nil }
