package wire

import "testing"

func FuzzDecodeTreeConnectResponse(f *testing.F) {
	fuzzCodec(f, sampleTreeConnectResponse(), EncodeTreeConnectResponse, func(data []byte) (TreeConnectResponse, error) {
		return DecodeTreeConnectResponse(Message{Header: Header{Command: TreeConnect, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeCloseRequest(f *testing.F) {
	fuzzCodec(f, sampleCloseRequest(), EncodeCloseRequest, func(data []byte) (CloseRequest, error) {
		return DecodeCloseRequest(Message{Header: Header{Command: Close, Flags: 0}, Body: data})
	})
}

func FuzzDecodeCloseResponse(f *testing.F) {
	fuzzCodec(f, sampleCloseResponse(), EncodeCloseResponse, func(data []byte) (CloseResponse, error) {
		return DecodeCloseResponse(Message{Header: Header{Command: Close, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeFlushRequest(f *testing.F) {
	fuzzCodec(f, sampleFlushRequest(), EncodeFlushRequest, func(data []byte) (FlushRequest, error) {
		return DecodeFlushRequest(Message{Header: Header{Command: Flush, Flags: 0}, Body: data})
	})
}

func FuzzDecodeWriteResponse(f *testing.F) {
	fuzzCodec(f, sampleWriteResponse(), EncodeWriteResponse, func(data []byte) (WriteResponse, error) {
		return DecodeWriteResponse(Message{Header: Header{Command: Write, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeChangeNotifyRequest(f *testing.F) {
	fuzzCodec(f, sampleChangeNotifyRequest(), EncodeChangeNotifyRequest, func(data []byte) (ChangeNotifyRequest, error) {
		return DecodeChangeNotifyRequest(Message{Header: Header{Command: ChangeNotify, Flags: 0}, Body: data})
	})
}

func FuzzDecodeLeaseBreakRequest(f *testing.F) {
	fuzzCodec(f, sampleLeaseBreakRequest(), EncodeLeaseBreakRequest, func(data []byte) (LeaseBreakRequest, error) {
		return DecodeLeaseBreakRequest(Message{Header: Header{Command: OplockBreak, Flags: 0}, Body: data})
	})
}

func FuzzDecodeLeaseBreakResponse(f *testing.F) {
	fuzzCodec(f, sampleLeaseBreakResponse(), EncodeLeaseBreakResponse, func(data []byte) (LeaseBreakResponse, error) {
		return DecodeLeaseBreakResponse(Message{Header: Header{Command: OplockBreak, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeLeaseBreakNotification(f *testing.F) {
	fuzzCodec(f, sampleLeaseBreakNotification(), EncodeLeaseBreakNotification, func(data []byte) (LeaseBreakNotification, error) {
		return DecodeLeaseBreakNotification(Message{Header: Header{Command: OplockBreak, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeEchoRequest(f *testing.F) {
	fuzzCodec(f, sampleEchoRequest(), EncodeEchoRequest, func(data []byte) (EmptyRequest, error) {
		return DecodeEchoRequest(Message{Header: Header{Command: Echo, Flags: 0}, Body: data})
	})
}

func FuzzDecodeEchoResponse(f *testing.F) {
	fuzzCodec(f, sampleEchoResponse(), EncodeEchoResponse, func(data []byte) (EmptyResponse, error) {
		return DecodeEchoResponse(Message{Header: Header{Command: Echo, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeLogoffRequest(f *testing.F) {
	fuzzCodec(f, sampleLogoffRequest(), EncodeLogoffRequest, func(data []byte) (EmptyRequest, error) {
		return DecodeLogoffRequest(Message{Header: Header{Command: Logoff, Flags: 0}, Body: data})
	})
}

func FuzzDecodeLogoffResponse(f *testing.F) {
	fuzzCodec(f, sampleLogoffResponse(), EncodeLogoffResponse, func(data []byte) (EmptyResponse, error) {
		return DecodeLogoffResponse(Message{Header: Header{Command: Logoff, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeTreeDisconnectRequest(f *testing.F) {
	fuzzCodec(f, sampleTreeDisconnectRequest(), EncodeTreeDisconnectRequest, func(data []byte) (EmptyRequest, error) {
		return DecodeTreeDisconnectRequest(Message{Header: Header{Command: TreeDisconnect, Flags: 0}, Body: data})
	})
}

func FuzzDecodeTreeDisconnectResponse(f *testing.F) {
	fuzzCodec(f, sampleTreeDisconnectResponse(), EncodeTreeDisconnectResponse, func(data []byte) (EmptyResponse, error) {
		return DecodeTreeDisconnectResponse(Message{Header: Header{Command: TreeDisconnect, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeCancelRequest(f *testing.F) {
	fuzzCodec(f, sampleCancelRequest(), EncodeCancelRequest, func(data []byte) (EmptyRequest, error) {
		return DecodeCancelRequest(Message{Header: Header{Command: Cancel, Flags: 0}, Body: data})
	})
}

func FuzzDecodeFlushResponse(f *testing.F) {
	fuzzCodec(f, sampleFlushResponse(), EncodeFlushResponse, func(data []byte) (EmptyResponse, error) {
		return DecodeFlushResponse(Message{Header: Header{Command: Flush, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeLockResponse(f *testing.F) {
	fuzzCodec(f, sampleLockResponse(), EncodeLockResponse, func(data []byte) (EmptyResponse, error) {
		return DecodeLockResponse(Message{Header: Header{Command: Lock, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeSetInfoResponse(f *testing.F) {
	fuzzCodec(f, sampleSetInfoResponse(), EncodeSetInfoResponse, func(data []byte) (EmptyResponse, error) {
		return DecodeSetInfoResponse(Message{Header: Header{Command: SetInfo, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeSessionSetupRequest(f *testing.F) {
	fuzzCodec(f, sampleSessionSetupRequest(), EncodeSessionSetupRequest, func(data []byte) (SessionSetupRequest, error) {
		return DecodeSessionSetupRequest(Message{Header: Header{Command: SessionSetup, Flags: 0}, Body: data})
	})
}

func FuzzDecodeSessionSetupResponse(f *testing.F) {
	fuzzCodec(f, sampleSessionSetupResponse(), EncodeSessionSetupResponse, func(data []byte) (SessionSetupResponse, error) {
		return DecodeSessionSetupResponse(Message{Header: Header{Command: SessionSetup, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeTreeConnectRequest(f *testing.F) {
	fuzzCodec(f, sampleTreeConnectRequest(), EncodeTreeConnectRequest, func(data []byte) (TreeConnectRequest, error) {
		return DecodeTreeConnectRequest(Message{Header: Header{Command: TreeConnect, Flags: 0}, Body: data})
	})
}

func FuzzDecodeCreateRequest(f *testing.F) {
	fuzzCodec(f, sampleCreateRequest(), EncodeCreateRequest, func(data []byte) (CreateRequest, error) {
		return DecodeCreateRequest(Message{Header: Header{Command: Create, Flags: 0}, Body: data})
	})
}

func FuzzDecodeCreateResponse(f *testing.F) {
	fuzzCodec(f, sampleCreateResponse(), EncodeCreateResponse, func(data []byte) (CreateResponse, error) {
		return DecodeCreateResponse(Message{Header: Header{Command: Create, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeReadRequest(f *testing.F) {
	fuzzCodec(f, sampleReadRequest(), EncodeReadRequest, func(data []byte) (ReadRequest, error) {
		return DecodeReadRequest(Message{Header: Header{Command: Read, Flags: 0}, Body: data})
	})
}

func FuzzDecodeReadResponse(f *testing.F) {
	fuzzCodec(f, sampleReadResponse(), EncodeReadResponse, func(data []byte) (ReadResponse, error) {
		return DecodeReadResponse(Message{Header: Header{Command: Read, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeWriteRequest(f *testing.F) {
	fuzzCodec(f, sampleWriteRequest(), EncodeWriteRequest, func(data []byte) (WriteRequest, error) {
		return DecodeWriteRequest(Message{Header: Header{Command: Write, Flags: 0}, Body: data})
	})
}

func FuzzDecodeLockRequest(f *testing.F) {
	fuzzCodec(f, sampleLockRequest(), EncodeLockRequest, func(data []byte) (LockRequest, error) {
		return DecodeLockRequest(Message{Header: Header{Command: Lock, Flags: 0}, Body: data})
	})
}

func FuzzDecodeQueryDirectoryRequest(f *testing.F) {
	fuzzCodec(f, sampleQueryDirectoryRequest(), EncodeQueryDirectoryRequest, func(data []byte) (QueryDirectoryRequest, error) {
		return DecodeQueryDirectoryRequest(Message{Header: Header{Command: QueryDirectory, Flags: 0}, Body: data})
	})
}

func FuzzDecodeQueryInfoRequest(f *testing.F) {
	fuzzCodec(f, sampleQueryInfoRequest(), EncodeQueryInfoRequest, func(data []byte) (QueryInfoRequest, error) {
		return DecodeQueryInfoRequest(Message{Header: Header{Command: QueryInfo, Flags: 0}, Body: data})
	})
}

func FuzzDecodeSetInfoRequest(f *testing.F) {
	fuzzCodec(f, sampleSetInfoRequest(), EncodeSetInfoRequest, func(data []byte) (SetInfoRequest, error) {
		return DecodeSetInfoRequest(Message{Header: Header{Command: SetInfo, Flags: 0}, Body: data})
	})
}

func FuzzDecodeQueryInfoResponse(f *testing.F) {
	fuzzCodec(f, sampleQueryInfoResponse(), EncodeQueryInfoResponse, func(data []byte) (QueryResponse, error) {
		return DecodeQueryInfoResponse(Message{Header: Header{Command: QueryInfo, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeQueryDirectoryResponse(f *testing.F) {
	fuzzCodec(f, sampleQueryDirectoryResponse(), EncodeQueryDirectoryResponse, func(data []byte) (QueryResponse, error) {
		return DecodeQueryDirectoryResponse(Message{Header: Header{Command: QueryDirectory, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeIOCTLRequest(f *testing.F) {
	fuzzCodec(f, sampleIOCTLRequest(), EncodeIOCTLRequest, func(data []byte) (IOCTLRequest, error) {
		return DecodeIOCTLRequest(Message{Header: Header{Command: IOCTL, Flags: 0}, Body: data})
	})
}

func FuzzDecodeIOCTLResponse(f *testing.F) {
	fuzzCodec(f, sampleIOCTLResponse(), EncodeIOCTLResponse, func(data []byte) (IOCTLResponse, error) {
		return DecodeIOCTLResponse(Message{Header: Header{Command: IOCTL, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeErrorResponse(f *testing.F) {
	fuzzCodec(f, sampleErrorResponse(), EncodeErrorResponse, func(data []byte) (ErrorResponse, error) {
		return DecodeErrorResponse(Message{Header: Header{Command: Echo, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeMaxAccessQuery(f *testing.F) {
	fuzzCodec(f, sampleMaxAccessQuery(), func(v MaxAccessQuery) ([]byte, error) { c, err := EncodeMaxAccessQuery(v); return c.Data, err }, func(data []byte) (MaxAccessQuery, error) {
		return DecodeMaxAccessQuery(CreateContext{Name: "MxAc", Data: data})
	})
}

func FuzzDecodeMaxAccessReply(f *testing.F) {
	fuzzCodec(f, sampleMaxAccessReply(), func(v MaxAccessReply) ([]byte, error) { c, err := EncodeMaxAccessReply(v); return c.Data, err }, func(data []byte) (MaxAccessReply, error) {
		return DecodeMaxAccessReply(CreateContext{Name: "MxAc", Data: data})
	})
}

func FuzzDecodeFileIDQuery(f *testing.F) {
	fuzzCodec(f, sampleFileIDQuery(), func(v FileIDQuery) ([]byte, error) { c, err := EncodeFileIDQuery(v); return c.Data, err }, func(data []byte) (FileIDQuery, error) {
		return DecodeFileIDQuery(CreateContext{Name: "QFid", Data: data})
	})
}

func FuzzDecodeFileIDReply(f *testing.F) {
	fuzzCodec(f, sampleFileIDReply(), func(v FileIDReply) ([]byte, error) { c, err := EncodeFileIDReply(v); return c.Data, err }, func(data []byte) (FileIDReply, error) {
		return DecodeFileIDReply(CreateContext{Name: "QFid", Data: data})
	})
}

func FuzzDecodeDurableRequest(f *testing.F) {
	fuzzCodec(f, sampleDurableRequest(), func(v DurableRequest) ([]byte, error) { c, err := EncodeDurableRequest(v); return c.Data, err }, func(data []byte) (DurableRequest, error) {
		return DecodeDurableRequest(CreateContext{Name: "DH2Q", Data: data})
	})
}

func FuzzDecodeDurableReply(f *testing.F) {
	fuzzCodec(f, sampleDurableReply(), func(v DurableReply) ([]byte, error) { c, err := EncodeDurableReply(v); return c.Data, err }, func(data []byte) (DurableReply, error) {
		return DecodeDurableReply(CreateContext{Name: "DH2Q", Data: data})
	})
}

func FuzzDecodeDurableReconnect(f *testing.F) {
	fuzzCodec(f, sampleDurableReconnect(), func(v DurableReconnect) ([]byte, error) { c, err := EncodeDurableReconnect(v); return c.Data, err }, func(data []byte) (DurableReconnect, error) {
		return DecodeDurableReconnect(CreateContext{Name: "DH2C", Data: data})
	})
}

func FuzzDecodePreauthContext(f *testing.F) {
	fuzzCodec(f, samplePreauthContext(), func(v PreauthContext) ([]byte, error) { c, err := EncodePreauthContext(v); return c.Data, err }, func(data []byte) (PreauthContext, error) {
		return DecodePreauthContext(NegotiateContext{Type: ContextPreauth, Data: data})
	})
}

func FuzzDecodeEncryptionContext(f *testing.F) {
	fuzzCodec(f, sampleEncryptionContext(), func(v EncryptionContext) ([]byte, error) { c, err := EncodeEncryptionContext(v); return c.Data, err }, func(data []byte) (EncryptionContext, error) {
		return DecodeEncryptionContext(NegotiateContext{Type: ContextEncryption, Data: data})
	})
}

func FuzzDecodeSigningContext(f *testing.F) {
	fuzzCodec(f, sampleSigningContext(), func(v SigningContext) ([]byte, error) { c, err := EncodeSigningContext(v); return c.Data, err }, func(data []byte) (SigningContext, error) {
		return DecodeSigningContext(NegotiateContext{Type: ContextSigning, Data: data})
	})
}

func FuzzDecodeAAPLQuery(f *testing.F) {
	fuzzCodec(f, sampleAAPLQuery(), func(v AAPLQuery) ([]byte, error) { c, err := EncodeAAPLQuery(v); return c.Data, err }, func(data []byte) (AAPLQuery, error) { return DecodeAAPLQuery(CreateContext{Name: "AAPL", Data: data}) })
}

func FuzzDecodeAAPLReply(f *testing.F) {
	fuzzCodec(f, sampleAAPLReply(), func(v AAPLReply) ([]byte, error) { c, err := EncodeAAPLReply(v); return c.Data, err }, func(data []byte) (AAPLReply, error) { return DecodeAAPLReply(CreateContext{Name: "AAPL", Data: data}) })
}

func FuzzDecodeLeaseContext(f *testing.F) {
	fuzzCodec(f, sampleLeaseContext(), func(v LeaseContext) ([]byte, error) { c, err := EncodeLeaseContext(v); return c.Data, err }, func(data []byte) (LeaseContext, error) {
		return DecodeLeaseContext(CreateContext{Name: "RqLs", Data: data})
	})
}

func FuzzDecodeDirectoryIDBothEntries(f *testing.F) {
	fuzzCodec(f, sampleDirectoryIDBothEntries(), EncodeDirectoryIDBothEntries, DecodeDirectoryIDBothEntries)
}

func FuzzDecodeDirectoryIDFullEntries(f *testing.F) {
	fuzzCodec(f, sampleDirectoryIDFullEntries(), EncodeDirectoryIDFullEntries, DecodeDirectoryIDFullEntries)
}

func FuzzDecodeHeader(f *testing.F) { fuzzCodec(f, sampleHeader(), EncodeHeader, DecodeHeader) }

func FuzzDecodeFileBasicInformation(f *testing.F) {
	fuzzCodec(f, sampleFileBasicInformation(), EncodeFileBasicInformation, DecodeFileBasicInformation)
}

func FuzzDecodeFileStandardInformation(f *testing.F) {
	fuzzCodec(f, sampleFileStandardInformation(), EncodeFileStandardInformation, DecodeFileStandardInformation)
}

func FuzzDecodeFileInternalInformation(f *testing.F) {
	fuzzCodec(f, sampleFileInternalInformation(), EncodeFileInternalInformation, DecodeFileInternalInformation)
}

func FuzzDecodeFileEAInformation(f *testing.F) {
	fuzzCodec(f, sampleFileEAInformation(), EncodeFileEAInformation, DecodeFileEAInformation)
}

func FuzzDecodeFileAccessInformation(f *testing.F) {
	fuzzCodec(f, sampleFileAccessInformation(), EncodeFileAccessInformation, DecodeFileAccessInformation)
}

func FuzzDecodeFilePositionInformation(f *testing.F) {
	fuzzCodec(f, sampleFilePositionInformation(), EncodeFilePositionInformation, DecodeFilePositionInformation)
}

func FuzzDecodeFileModeInformation(f *testing.F) {
	fuzzCodec(f, sampleFileModeInformation(), EncodeFileModeInformation, DecodeFileModeInformation)
}

func FuzzDecodeFileAlignmentInformation(f *testing.F) {
	fuzzCodec(f, sampleFileAlignmentInformation(), EncodeFileAlignmentInformation, DecodeFileAlignmentInformation)
}

func FuzzDecodeFileNetworkOpenInformation(f *testing.F) {
	fuzzCodec(f, sampleFileNetworkOpenInformation(), EncodeFileNetworkOpenInformation, DecodeFileNetworkOpenInformation)
}

func FuzzDecodeFileAttributeTagInformation(f *testing.F) {
	fuzzCodec(f, sampleFileAttributeTagInformation(), EncodeFileAttributeTagInformation, DecodeFileAttributeTagInformation)
}

func FuzzDecodeFileIDInformation(f *testing.F) {
	fuzzCodec(f, sampleFileIDInformation(), EncodeFileIDInformation, DecodeFileIDInformation)
}

func FuzzDecodeFileDispositionInformation(f *testing.F) {
	fuzzCodec(f, sampleFileDispositionInformation(), EncodeFileDispositionInformation, DecodeFileDispositionInformation)
}

func FuzzDecodeFileEndOfFileInformation(f *testing.F) {
	fuzzCodec(f, sampleFileEndOfFileInformation(), EncodeFileEndOfFileInformation, DecodeFileEndOfFileInformation)
}

func FuzzDecodeFileAllocationInformation(f *testing.F) {
	fuzzCodec(f, sampleFileAllocationInformation(), EncodeFileAllocationInformation, DecodeFileAllocationInformation)
}

func FuzzDecodeFilesystemSizeInformation(f *testing.F) {
	fuzzCodec(f, sampleFilesystemSizeInformation(), EncodeFilesystemSizeInformation, DecodeFilesystemSizeInformation)
}

func FuzzDecodeFilesystemFullSizeInformation(f *testing.F) {
	fuzzCodec(f, sampleFilesystemFullSizeInformation(), EncodeFilesystemFullSizeInformation, DecodeFilesystemFullSizeInformation)
}

func FuzzDecodeFilesystemDeviceInformation(f *testing.F) {
	fuzzCodec(f, sampleFilesystemDeviceInformation(), EncodeFilesystemDeviceInformation, DecodeFilesystemDeviceInformation)
}

func FuzzDecodeFileNameInformation(f *testing.F) {
	fuzzCodec(f, sampleFileNameInformation(), EncodeFileNameInformation, DecodeFileNameInformation)
}

func FuzzDecodeFileAllInformation(f *testing.F) {
	fuzzCodec(f, sampleFileAllInformation(), EncodeFileAllInformation, DecodeFileAllInformation)
}

func FuzzDecodeFileRenameInformation(f *testing.F) {
	fuzzCodec(f, sampleFileRenameInformation(), EncodeFileRenameInformation, DecodeFileRenameInformation)
}

func FuzzDecodeFilesystemVolumeInformation(f *testing.F) {
	fuzzCodec(f, sampleFilesystemVolumeInformation(), EncodeFilesystemVolumeInformation, DecodeFilesystemVolumeInformation)
}

func FuzzDecodeFilesystemAttributeInformation(f *testing.F) {
	fuzzCodec(f, sampleFilesystemAttributeInformation(), EncodeFilesystemAttributeInformation, DecodeFilesystemAttributeInformation)
}

func FuzzDecodeFileStreamInformation(f *testing.F) {
	fuzzCodec(f, sampleFileStreamInformation(), EncodeFileStreamInformation, DecodeFileStreamInformation)
}

func FuzzDecodeNegotiateRequest(f *testing.F) {
	fuzzCodec(f, sampleNegotiateRequest(), EncodeNegotiateRequest, func(data []byte) (NegotiateRequest, error) {
		return DecodeNegotiateRequest(Message{Header: Header{Command: Negotiate, Flags: 0}, Body: data})
	})
}

func FuzzDecodeNegotiateResponse(f *testing.F) {
	fuzzCodec(f, sampleNegotiateResponse(), EncodeNegotiateResponse, func(data []byte) (NegotiateResponse, error) {
		return DecodeNegotiateResponse(Message{Header: Header{Command: Negotiate, Flags: FlagResponse}, Body: data})
	})
}

func FuzzDecodeSID(f *testing.F) { fuzzCodec(f, sampleSID(), EncodeSID, DecodeSID) }

func FuzzDecodeACL(f *testing.F) { fuzzCodec(f, sampleACL(), EncodeACL, DecodeACL) }

func FuzzDecodeSecurityDescriptor(f *testing.F) {
	fuzzCodec(f, sampleSecurityDescriptor(), EncodeSecurityDescriptor, DecodeSecurityDescriptor)
}
