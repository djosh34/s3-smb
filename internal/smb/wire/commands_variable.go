package wire

// DecodeSessionSetupRequest validates fixed fields and variable buffers.
func DecodeSessionSetupRequest(m Message) (SessionSetupRequest, error) {
	r := body(m, SessionSetup, false, 25, 24)
	var v SessionSetupRequest
	v.Flags = r.u8()
	v.SecurityMode = r.u8()
	v.Capabilities = r.u32()
	r.skip(4)
	offset := r.u16()
	length := r.u16()
	v.PreviousSessionID = r.u64()
	v.Token = clone(r.field(uint64(offset), uint64(length), 24, 1))
	return v, r.err
}

// EncodeSessionSetupRequest writes the command body without an SMB header.
func EncodeSessionSetupRequest(v SessionSetupRequest) ([]byte, error) {
	b := builder{}
	b.u16(25)
	b.u8(v.Flags)
	b.u8(v.SecurityMode)
	b.u32(v.Capabilities)
	b.zero(4)
	if len(v.Token) > 0 {
		b.u16(88)
	} else {
		b.u16(0)
	}
	b.length16(len(v.Token))
	b.u64(v.PreviousSessionID)
	b.bytes(v.Token)
	return b.finish()
}

// DecodeSessionSetupResponse validates fixed fields and variable buffers.
func DecodeSessionSetupResponse(m Message) (SessionSetupResponse, error) {
	r := body(m, SessionSetup, true, 9, 8)
	var v SessionSetupResponse
	v.Flags = r.u16()
	offset := r.u16()
	length := r.u16()
	v.Token = clone(r.field(uint64(offset), uint64(length), 8, 1))
	return v, r.err
}

// EncodeSessionSetupResponse writes the command body without an SMB header.
func EncodeSessionSetupResponse(v SessionSetupResponse) ([]byte, error) {
	b := builder{}
	b.u16(9)
	b.u16(v.Flags)
	if len(v.Token) > 0 {
		b.u16(72)
	} else {
		b.u16(0)
	}
	b.length16(len(v.Token))
	b.bytes(v.Token)
	return b.finish()
}

// DecodeTreeConnectRequest validates fixed fields and variable buffers.
func DecodeTreeConnectRequest(m Message) (TreeConnectRequest, error) {
	r := body(m, TreeConnect, false, 9, 8)
	var v TreeConnectRequest
	v.Flags = r.u16()
	offset := r.u16()
	length := r.u16()
	v.Path = r.text(r.field(uint64(offset), uint64(length), 8, 2))
	return v, r.err
}

// EncodeTreeConnectRequest writes the command body without an SMB header.
func EncodeTreeConnectRequest(v TreeConnectRequest) ([]byte, error) {
	name, err := encodeUTF16(v.Path)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.u16(9)
	b.u16(v.Flags)
	if len(name) > 0 {
		b.u16(72)
	} else {
		b.u16(0)
	}
	b.length16(len(name))
	b.bytes(name)
	return b.finish()
}

// DecodeCreateRequest validates fixed fields and variable buffers.
func DecodeCreateRequest(m Message) (CreateRequest, error) {
	r := body(m, Create, false, 57, 57)
	var v CreateRequest
	r.skip(1)
	v.OplockLevel = r.u8()
	v.ImpersonationLevel = r.u32()
	r.skip(16)
	v.DesiredAccess = r.u32()
	v.FileAttributes = r.u32()
	v.ShareAccess = r.u32()
	v.Disposition = r.u32()
	v.Options = r.u32()
	nameOffset := r.u16()
	nameLength := r.u16()
	contextOffset := r.u32()
	contextLength := r.u32()
	v.Name = r.text(r.field(uint64(nameOffset), uint64(nameLength), 56, 8))
	v.Contexts = r.createContexts(contextOffset, contextLength, 56)
	return v, r.err
}

// EncodeCreateRequest writes the command body without an SMB header.
func EncodeCreateRequest(v CreateRequest) ([]byte, error) {
	name, err := encodeUTF16(v.Name)
	if err != nil {
		return nil, err
	}
	contexts, err := encodeCreateContexts(v.Contexts)
	if err != nil {
		return nil, err
	}
	offset := 56 + len(name)
	offset += (8 - offset%8) % 8
	b := builder{}
	b.u16(57)
	b.zero(1)
	b.u8(v.OplockLevel)
	b.u32(v.ImpersonationLevel)
	b.zero(16)
	b.u32(v.DesiredAccess)
	b.u32(v.FileAttributes)
	b.u32(v.ShareAccess)
	b.u32(v.Disposition)
	b.u32(v.Options)
	if len(name) > 0 {
		b.u16(120)
	} else {
		b.u16(0)
	}
	b.length16(len(name))
	if len(contexts) > 0 {
		b.length32(offset + 64)
	} else {
		b.u32(0)
	}
	b.length32(len(contexts))
	b.bytes(name)
	if len(contexts) > 0 {
		b.align8()
		b.bytes(contexts)
	} else if len(name) == 0 {
		b.u8(0)
	}
	return b.finish()
}

// DecodeCreateResponse validates fixed fields and variable buffers.
func DecodeCreateResponse(m Message) (CreateResponse, error) {
	r := body(m, Create, true, 89, 88)
	var v CreateResponse
	v.OplockLevel = r.u8()
	v.Flags = r.u8()
	v.Action = r.u32()
	v.Created = r.u64()
	v.Accessed = r.u64()
	v.Modified = r.u64()
	v.Changed = r.u64()
	v.AllocationSize = r.u64()
	v.Size = r.u64()
	v.Attributes = r.u32()
	r.skip(4)
	v.ID = r.id()
	offset := r.u32()
	length := r.u32()
	v.Contexts = r.createContexts(offset, length, 88)
	return v, r.err
}

// EncodeCreateResponse writes the command body without an SMB header.
func EncodeCreateResponse(v CreateResponse) ([]byte, error) {
	contexts, err := encodeCreateContexts(v.Contexts)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.u16(89)
	b.u8(v.OplockLevel)
	b.u8(v.Flags)
	b.u32(v.Action)
	b.u64(v.Created)
	b.u64(v.Accessed)
	b.u64(v.Modified)
	b.u64(v.Changed)
	b.u64(v.AllocationSize)
	b.u64(v.Size)
	b.u32(v.Attributes)
	b.zero(4)
	b.id(v.ID)
	if len(contexts) > 0 {
		b.u32(152)
	} else {
		b.u32(0)
	}
	b.length32(len(contexts))
	b.bytes(contexts)
	return b.finish()
}

// DecodeReadRequest validates fixed fields and variable buffers.
func DecodeReadRequest(m Message) (ReadRequest, error) {
	r := body(m, Read, false, 49, 48)
	var v ReadRequest
	r.skip(1)
	v.Flags = r.u8()
	v.Length = r.u32()
	v.Offset = r.u64()
	v.ID = r.id()
	v.MinimumCount = r.u32()
	v.Channel = r.u32()
	v.RemainingBytes = r.u32()
	offset := r.u16()
	length := r.u16()
	v.ChannelInfo = clone(r.field(uint64(offset), uint64(length), 48, 1))
	return v, r.err
}

// EncodeReadRequest writes the command body without an SMB header.
func EncodeReadRequest(v ReadRequest) ([]byte, error) {
	b := builder{}
	b.u16(49)
	b.zero(1)
	b.u8(v.Flags)
	b.u32(v.Length)
	b.u64(v.Offset)
	b.id(v.ID)
	b.u32(v.MinimumCount)
	b.u32(v.Channel)
	b.u32(v.RemainingBytes)
	if len(v.ChannelInfo) > 0 {
		b.u16(112)
	} else {
		b.u16(0)
	}
	b.length16(len(v.ChannelInfo))
	b.bytes(v.ChannelInfo)
	return b.finish()
}

// DecodeReadResponse validates fixed fields and variable buffers.
func DecodeReadResponse(m Message) (ReadResponse, error) {
	r := body(m, Read, true, 17, 16)
	var v ReadResponse
	offset := r.u8()
	r.skip(1)
	length := r.u32()
	v.Remaining = r.u32()
	r.skip(4)
	v.Data = clone(r.field(uint64(offset), uint64(length), 16, 1))
	return v, r.err
}

// EncodeReadResponse writes the command body without an SMB header.
func EncodeReadResponse(v ReadResponse) ([]byte, error) {
	b := builder{}
	b.u16(17)
	if len(v.Data) > 0 {
		b.u8(80)
	} else {
		b.u8(0)
	}
	b.u8(0)
	b.length32(len(v.Data))
	b.u32(v.Remaining)
	b.u32(0)
	b.bytes(v.Data)
	return b.finish()
}

// DecodeWriteRequest validates fixed fields and variable buffers.
func DecodeWriteRequest(m Message) (WriteRequest, error) {
	r := body(m, Write, false, 49, 48)
	var v WriteRequest
	offset := r.u16()
	length := r.u32()
	v.Offset = r.u64()
	v.ID = r.id()
	v.Channel = r.u32()
	v.RemainingBytes = r.u32()
	channelOffset := r.u16()
	channelLength := r.u16()
	v.Flags = r.u32()
	v.Data = clone(r.field(uint64(offset), uint64(length), 48, 1))
	v.ChannelInfo = clone(r.field(uint64(channelOffset), uint64(channelLength), 48, 1))
	return v, r.err
}

// EncodeWriteRequest writes the command body without an SMB header.
func EncodeWriteRequest(v WriteRequest) ([]byte, error) {
	b := builder{}
	b.u16(49)
	if len(v.Data) > 0 {
		b.u16(112)
	} else {
		b.u16(0)
	}
	b.length32(len(v.Data))
	b.u64(v.Offset)
	b.id(v.ID)
	b.u32(v.Channel)
	b.u32(v.RemainingBytes)
	if len(v.ChannelInfo) > 0 {
		b.length16(112 + len(v.Data))
	} else {
		b.u16(0)
	}
	b.length16(len(v.ChannelInfo))
	b.u32(v.Flags)
	b.bytes(v.Data)
	b.bytes(v.ChannelInfo)
	return b.finish()
}

// DecodeQueryDirectoryRequest validates fixed fields and variable buffers.
func DecodeQueryDirectoryRequest(m Message) (QueryDirectoryRequest, error) {
	r := body(m, QueryDirectory, false, 33, 32)
	var v QueryDirectoryRequest
	v.InfoClass = DirectoryInfoClass(r.u8())
	v.Flags = r.u8()
	v.FileIndex = r.u32()
	v.ID = r.id()
	offset := r.u16()
	length := r.u16()
	v.OutputLength = r.u32()
	v.Pattern = r.text(r.field(uint64(offset), uint64(length), 32, 2))
	return v, r.err
}

// EncodeQueryDirectoryRequest writes the command body without an SMB header.
func EncodeQueryDirectoryRequest(v QueryDirectoryRequest) ([]byte, error) {
	name, err := encodeUTF16(v.Pattern)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.u16(33)
	b.u8(uint8(v.InfoClass))
	b.u8(v.Flags)
	b.u32(v.FileIndex)
	b.id(v.ID)
	if len(name) > 0 {
		b.u16(96)
	} else {
		b.u16(0)
	}
	b.length16(len(name))
	b.u32(v.OutputLength)
	b.bytes(name)
	return b.finish()
}

// DecodeQueryInfoRequest validates fixed fields and variable buffers.
func DecodeQueryInfoRequest(m Message) (QueryInfoRequest, error) {
	r := body(m, QueryInfo, false, 41, 40)
	var v QueryInfoRequest
	v.InfoType = InfoType(r.u8())
	v.InfoClass = r.u8()
	v.OutputLength = r.u32()
	offset := r.u16()
	r.skip(2)
	length := r.u32()
	v.AdditionalInformation = r.u32()
	v.Flags = r.u32()
	v.ID = r.id()
	v.Input = clone(r.field(uint64(offset), uint64(length), 40, 1))
	return v, r.err
}

// EncodeQueryInfoRequest writes the command body without an SMB header.
func EncodeQueryInfoRequest(v QueryInfoRequest) ([]byte, error) {
	b := builder{}
	b.u16(41)
	b.u8(uint8(v.InfoType))
	b.u8(v.InfoClass)
	b.u32(v.OutputLength)
	if len(v.Input) > 0 {
		b.u16(104)
	} else {
		b.u16(0)
	}
	b.u16(0)
	b.length32(len(v.Input))
	b.u32(v.AdditionalInformation)
	b.u32(v.Flags)
	b.id(v.ID)
	b.bytes(v.Input)
	return b.finish()
}

// DecodeSetInfoRequest validates fixed fields and variable buffers.
func DecodeSetInfoRequest(m Message) (SetInfoRequest, error) {
	r := body(m, SetInfo, false, 33, 32)
	var v SetInfoRequest
	v.InfoType = InfoType(r.u8())
	v.InfoClass = r.u8()
	length := r.u32()
	offset := r.u16()
	r.skip(2)
	v.AdditionalInformation = r.u32()
	v.ID = r.id()
	v.Input = clone(r.field(uint64(offset), uint64(length), 32, 1))
	return v, r.err
}

// EncodeSetInfoRequest writes the command body without an SMB header.
func EncodeSetInfoRequest(v SetInfoRequest) ([]byte, error) {
	b := builder{}
	b.u16(33)
	b.u8(uint8(v.InfoType))
	b.u8(v.InfoClass)
	b.length32(len(v.Input))
	if len(v.Input) > 0 {
		b.u16(96)
	} else {
		b.u16(0)
	}
	b.u16(0)
	b.u32(v.AdditionalInformation)
	b.id(v.ID)
	b.bytes(v.Input)
	return b.finish()
}

// DecodeQueryInfoResponse validates fixed fields and variable buffers.
func DecodeQueryInfoResponse(m Message) (QueryResponse, error) {
	r := body(m, QueryInfo, true, 9, 8)
	var v QueryResponse
	offset := r.u16()
	length := r.u32()
	v.Data = clone(r.field(uint64(offset), uint64(length), 8, 1))
	return v, r.err
}

// EncodeQueryInfoResponse writes the command body without an SMB header.
func EncodeQueryInfoResponse(v QueryResponse) ([]byte, error) {
	b := builder{}
	b.u16(9)
	if len(v.Data) > 0 {
		b.u16(72)
	} else {
		b.u16(0)
	}
	b.length32(len(v.Data))
	b.bytes(v.Data)
	return b.finish()
}

// DecodeQueryDirectoryResponse validates fixed fields and variable buffers.
func DecodeQueryDirectoryResponse(m Message) (QueryResponse, error) {
	r := body(m, QueryDirectory, true, 9, 8)
	var v QueryResponse
	offset := r.u16()
	length := r.u32()
	v.Data = clone(r.field(uint64(offset), uint64(length), 8, 1))
	return v, r.err
}

// EncodeQueryDirectoryResponse writes the command body without an SMB header.
func EncodeQueryDirectoryResponse(v QueryResponse) ([]byte, error) {
	b := builder{}
	b.u16(9)
	if len(v.Data) > 0 {
		b.u16(72)
	} else {
		b.u16(0)
	}
	b.length32(len(v.Data))
	b.bytes(v.Data)
	return b.finish()
}

// DecodeIOCTLRequest validates fixed fields and variable buffers.
func DecodeIOCTLRequest(m Message) (IOCTLRequest, error) {
	r := body(m, IOCTL, false, 57, 56)
	var v IOCTLRequest
	r.skip(2)
	v.ControlCode = r.u32()
	v.ID = r.id()
	inputOffset := r.u32()
	inputLength := r.u32()
	r.skip(4)
	outputOffset := r.u32()
	outputLength := r.u32()
	v.MaxOutput = r.u32()
	v.Flags = r.u32()
	r.skip(4)
	v.Input = clone(r.field(uint64(inputOffset), uint64(inputLength), 56, 1))
	r.field(uint64(outputOffset), uint64(outputLength), 56, 1)
	return v, r.err
}

// EncodeIOCTLRequest writes the command body without an SMB header.
func EncodeIOCTLRequest(v IOCTLRequest) ([]byte, error) {
	b := builder{}
	b.u16(57)
	b.zero(2)
	b.u32(v.ControlCode)
	b.id(v.ID)
	if len(v.Input) > 0 {
		b.u32(120)
	} else {
		b.u32(0)
	}
	b.length32(len(v.Input))
	b.u32(0)
	b.u32(0)
	b.u32(0)
	b.u32(v.MaxOutput)
	b.u32(v.Flags)
	b.u32(0)
	b.bytes(v.Input)
	return b.finish()
}

// DecodeErrorResponse validates fixed fields and variable buffers.
func DecodeErrorResponse(m Message) (ErrorResponse, error) {
	r := body(m, m.Header.Command, true, 9, 8)
	var v ErrorResponse
	v.ContextCount = r.u8()
	r.skip(1)
	length := r.u32()
	v.Data = clone(r.region(8, uint64(length), 8, 1))
	if r.err != nil {
		return ErrorResponse{}, r.err
	}
	if err := validateErrorContexts(v); err != nil {
		return ErrorResponse{}, err
	}
	return v, nil
}

// EncodeErrorResponse writes the command body without an SMB header.
func EncodeErrorResponse(v ErrorResponse) ([]byte, error) {
	if err := validateErrorContexts(v); err != nil {
		return nil, err
	}
	b := builder{}
	b.u16(9)
	b.u8(v.ContextCount)
	b.zero(1)
	b.length32(len(v.Data))
	b.bytes(v.Data)
	if len(v.Data) == 0 {
		b.u8(0)
	}
	return b.finish()
}

func validateErrorContexts(v ErrorResponse) error {
	if v.ContextCount == 0 {
		return nil
	}
	r := reader{data: v.Data}
	for i := uint8(0); i < v.ContextCount; i++ {
		if i > 0 {
			r.skip((8 - r.pos%8) % 8)
		}
		n := r.u32()
		r.skip(4)
		if uint64(n) > uint64(len(v.Data)) {
			return errMalformed
		}
		r.skip(int(n))
	}
	return r.exact()
}
