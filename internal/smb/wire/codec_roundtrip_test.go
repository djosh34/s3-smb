package wire

import (
	"testing"
)

func TestTypedCodecRoundTrips(t *testing.T) {
	t.Run("TreeConnectResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleTreeConnectResponse(), EncodeTreeConnectResponse, func(data []byte) (TreeConnectResponse, error) {
			return DecodeTreeConnectResponse(Message{Header: Header{Command: TreeConnect, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleTreeConnectResponse(), EncodeTreeConnectResponse, DecodeTreeConnectResponse, TreeConnect, true)
	})
	t.Run("CloseRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleCloseRequest(), EncodeCloseRequest, func(data []byte) (CloseRequest, error) {
			return DecodeCloseRequest(Message{Header: Header{Command: Close, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleCloseRequest(), EncodeCloseRequest, DecodeCloseRequest, Close, false)
	})
	t.Run("CloseResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleCloseResponse(), EncodeCloseResponse, func(data []byte) (CloseResponse, error) {
			return DecodeCloseResponse(Message{Header: Header{Command: Close, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleCloseResponse(), EncodeCloseResponse, DecodeCloseResponse, Close, true)
	})
	t.Run("FlushRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleFlushRequest(), EncodeFlushRequest, func(data []byte) (FlushRequest, error) {
			return DecodeFlushRequest(Message{Header: Header{Command: Flush, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleFlushRequest(), EncodeFlushRequest, DecodeFlushRequest, Flush, false)
	})
	t.Run("WriteResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleWriteResponse(), EncodeWriteResponse, func(data []byte) (WriteResponse, error) {
			return DecodeWriteResponse(Message{Header: Header{Command: Write, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleWriteResponse(), EncodeWriteResponse, DecodeWriteResponse, Write, true)
	})
	t.Run("ChangeNotifyRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleChangeNotifyRequest(), EncodeChangeNotifyRequest, func(data []byte) (ChangeNotifyRequest, error) {
			return DecodeChangeNotifyRequest(Message{Header: Header{Command: ChangeNotify, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleChangeNotifyRequest(), EncodeChangeNotifyRequest, DecodeChangeNotifyRequest, ChangeNotify, false)
	})
	t.Run("LeaseBreakRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleLeaseBreakRequest(), EncodeLeaseBreakRequest, func(data []byte) (LeaseBreakRequest, error) {
			return DecodeLeaseBreakRequest(Message{Header: Header{Command: OplockBreak, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleLeaseBreakRequest(), EncodeLeaseBreakRequest, DecodeLeaseBreakRequest, OplockBreak, false)
	})
	t.Run("LeaseBreakResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleLeaseBreakResponse(), EncodeLeaseBreakResponse, func(data []byte) (LeaseBreakResponse, error) {
			return DecodeLeaseBreakResponse(Message{Header: Header{Command: OplockBreak, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleLeaseBreakResponse(), EncodeLeaseBreakResponse, DecodeLeaseBreakResponse, OplockBreak, true)
	})
	t.Run("LeaseBreakNotification", func(t *testing.T) {
		checkRoundTrip(t, sampleLeaseBreakNotification(), EncodeLeaseBreakNotification, func(data []byte) (LeaseBreakNotification, error) {
			return DecodeLeaseBreakNotification(Message{Header: Header{Command: OplockBreak, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleLeaseBreakNotification(), EncodeLeaseBreakNotification, DecodeLeaseBreakNotification, OplockBreak, true)
	})
	t.Run("EchoRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleEchoRequest(), EncodeEchoRequest, func(data []byte) (EmptyRequest, error) {
			return DecodeEchoRequest(Message{Header: Header{Command: Echo, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleEchoRequest(), EncodeEchoRequest, DecodeEchoRequest, Echo, false)
	})
	t.Run("EchoResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleEchoResponse(), EncodeEchoResponse, func(data []byte) (EmptyResponse, error) {
			return DecodeEchoResponse(Message{Header: Header{Command: Echo, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleEchoResponse(), EncodeEchoResponse, DecodeEchoResponse, Echo, true)
	})
	t.Run("LogoffRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleLogoffRequest(), EncodeLogoffRequest, func(data []byte) (EmptyRequest, error) {
			return DecodeLogoffRequest(Message{Header: Header{Command: Logoff, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleLogoffRequest(), EncodeLogoffRequest, DecodeLogoffRequest, Logoff, false)
	})
	t.Run("LogoffResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleLogoffResponse(), EncodeLogoffResponse, func(data []byte) (EmptyResponse, error) {
			return DecodeLogoffResponse(Message{Header: Header{Command: Logoff, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleLogoffResponse(), EncodeLogoffResponse, DecodeLogoffResponse, Logoff, true)
	})
	t.Run("TreeDisconnectRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleTreeDisconnectRequest(), EncodeTreeDisconnectRequest, func(data []byte) (EmptyRequest, error) {
			return DecodeTreeDisconnectRequest(Message{Header: Header{Command: TreeDisconnect, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleTreeDisconnectRequest(), EncodeTreeDisconnectRequest, DecodeTreeDisconnectRequest, TreeDisconnect, false)
	})
	t.Run("TreeDisconnectResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleTreeDisconnectResponse(), EncodeTreeDisconnectResponse, func(data []byte) (EmptyResponse, error) {
			return DecodeTreeDisconnectResponse(Message{Header: Header{Command: TreeDisconnect, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleTreeDisconnectResponse(), EncodeTreeDisconnectResponse, DecodeTreeDisconnectResponse, TreeDisconnect, true)
	})
	t.Run("CancelRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleCancelRequest(), EncodeCancelRequest, func(data []byte) (EmptyRequest, error) {
			return DecodeCancelRequest(Message{Header: Header{Command: Cancel, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleCancelRequest(), EncodeCancelRequest, DecodeCancelRequest, Cancel, false)
	})
	t.Run("FlushResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleFlushResponse(), EncodeFlushResponse, func(data []byte) (EmptyResponse, error) {
			return DecodeFlushResponse(Message{Header: Header{Command: Flush, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleFlushResponse(), EncodeFlushResponse, DecodeFlushResponse, Flush, true)
	})
	t.Run("LockResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleLockResponse(), EncodeLockResponse, func(data []byte) (EmptyResponse, error) {
			return DecodeLockResponse(Message{Header: Header{Command: Lock, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleLockResponse(), EncodeLockResponse, DecodeLockResponse, Lock, true)
	})
	t.Run("SetInfoResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleSetInfoResponse(), EncodeSetInfoResponse, func(data []byte) (EmptyResponse, error) {
			return DecodeSetInfoResponse(Message{Header: Header{Command: SetInfo, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleSetInfoResponse(), EncodeSetInfoResponse, DecodeSetInfoResponse, SetInfo, true)
	})
	t.Run("SessionSetupRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleSessionSetupRequest(), EncodeSessionSetupRequest, func(data []byte) (SessionSetupRequest, error) {
			return DecodeSessionSetupRequest(Message{Header: Header{Command: SessionSetup, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleSessionSetupRequest(), EncodeSessionSetupRequest, DecodeSessionSetupRequest, SessionSetup, false)
	})
	t.Run("SessionSetupResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleSessionSetupResponse(), EncodeSessionSetupResponse, func(data []byte) (SessionSetupResponse, error) {
			return DecodeSessionSetupResponse(Message{Header: Header{Command: SessionSetup, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleSessionSetupResponse(), EncodeSessionSetupResponse, DecodeSessionSetupResponse, SessionSetup, true)
	})
	t.Run("TreeConnectRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleTreeConnectRequest(), EncodeTreeConnectRequest, func(data []byte) (TreeConnectRequest, error) {
			return DecodeTreeConnectRequest(Message{Header: Header{Command: TreeConnect, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleTreeConnectRequest(), EncodeTreeConnectRequest, DecodeTreeConnectRequest, TreeConnect, false)
	})
	t.Run("CreateRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleCreateRequest(), EncodeCreateRequest, func(data []byte) (CreateRequest, error) {
			return DecodeCreateRequest(Message{Header: Header{Command: Create, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleCreateRequest(), EncodeCreateRequest, DecodeCreateRequest, Create, false)
	})
	t.Run("CreateResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleCreateResponse(), EncodeCreateResponse, func(data []byte) (CreateResponse, error) {
			return DecodeCreateResponse(Message{Header: Header{Command: Create, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleCreateResponse(), EncodeCreateResponse, DecodeCreateResponse, Create, true)
	})
	t.Run("ReadRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleReadRequest(), EncodeReadRequest, func(data []byte) (ReadRequest, error) {
			return DecodeReadRequest(Message{Header: Header{Command: Read, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleReadRequest(), EncodeReadRequest, DecodeReadRequest, Read, false)
	})
	t.Run("ReadResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleReadResponse(), EncodeReadResponse, func(data []byte) (ReadResponse, error) {
			return DecodeReadResponse(Message{Header: Header{Command: Read, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleReadResponse(), EncodeReadResponse, DecodeReadResponse, Read, true)
	})
	t.Run("WriteRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleWriteRequest(), EncodeWriteRequest, func(data []byte) (WriteRequest, error) {
			return DecodeWriteRequest(Message{Header: Header{Command: Write, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleWriteRequest(), EncodeWriteRequest, DecodeWriteRequest, Write, false)
	})
	t.Run("LockRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleLockRequest(), EncodeLockRequest, func(data []byte) (LockRequest, error) {
			return DecodeLockRequest(Message{Header: Header{Command: Lock, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleLockRequest(), EncodeLockRequest, DecodeLockRequest, Lock, false)
	})
	t.Run("QueryDirectoryRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleQueryDirectoryRequest(), EncodeQueryDirectoryRequest, func(data []byte) (QueryDirectoryRequest, error) {
			return DecodeQueryDirectoryRequest(Message{Header: Header{Command: QueryDirectory, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleQueryDirectoryRequest(), EncodeQueryDirectoryRequest, DecodeQueryDirectoryRequest, QueryDirectory, false)
	})
	t.Run("QueryInfoRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleQueryInfoRequest(), EncodeQueryInfoRequest, func(data []byte) (QueryInfoRequest, error) {
			return DecodeQueryInfoRequest(Message{Header: Header{Command: QueryInfo, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleQueryInfoRequest(), EncodeQueryInfoRequest, DecodeQueryInfoRequest, QueryInfo, false)
	})
	t.Run("SetInfoRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleSetInfoRequest(), EncodeSetInfoRequest, func(data []byte) (SetInfoRequest, error) {
			return DecodeSetInfoRequest(Message{Header: Header{Command: SetInfo, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleSetInfoRequest(), EncodeSetInfoRequest, DecodeSetInfoRequest, SetInfo, false)
	})
	t.Run("QueryInfoResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleQueryInfoResponse(), EncodeQueryInfoResponse, func(data []byte) (QueryResponse, error) {
			return DecodeQueryInfoResponse(Message{Header: Header{Command: QueryInfo, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleQueryInfoResponse(), EncodeQueryInfoResponse, DecodeQueryInfoResponse, QueryInfo, true)
	})
	t.Run("QueryDirectoryResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleQueryDirectoryResponse(), EncodeQueryDirectoryResponse, func(data []byte) (QueryResponse, error) {
			return DecodeQueryDirectoryResponse(Message{Header: Header{Command: QueryDirectory, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleQueryDirectoryResponse(), EncodeQueryDirectoryResponse, DecodeQueryDirectoryResponse, QueryDirectory, true)
	})
	t.Run("IOCTLRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleIOCTLRequest(), EncodeIOCTLRequest, func(data []byte) (IOCTLRequest, error) {
			return DecodeIOCTLRequest(Message{Header: Header{Command: IOCTL, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleIOCTLRequest(), EncodeIOCTLRequest, DecodeIOCTLRequest, IOCTL, false)
	})
	t.Run("IOCTLResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleIOCTLResponse(), EncodeIOCTLResponse, func(data []byte) (IOCTLResponse, error) {
			return DecodeIOCTLResponse(Message{Header: Header{Command: IOCTL, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleIOCTLResponse(), EncodeIOCTLResponse, DecodeIOCTLResponse, IOCTL, true)
	})
	t.Run("ErrorResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleErrorResponse(), EncodeErrorResponse, func(data []byte) (ErrorResponse, error) {
			return DecodeErrorResponse(Message{Header: Header{Command: Echo, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleErrorResponse(), EncodeErrorResponse, DecodeErrorResponse, Echo, true)
	})
	t.Run("MaxAccessQuery", func(t *testing.T) {
		checkRoundTrip(t, sampleMaxAccessQuery(), func(v MaxAccessQuery) ([]byte, error) { c, err := EncodeMaxAccessQuery(v); return c.Data, err }, func(data []byte) (MaxAccessQuery, error) {
			return DecodeMaxAccessQuery(CreateContext{Name: "MxAc", Data: data})
		})
		if _, err := DecodeMaxAccessQuery(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("MaxAccessReply", func(t *testing.T) {
		checkRoundTrip(t, sampleMaxAccessReply(), func(v MaxAccessReply) ([]byte, error) { c, err := EncodeMaxAccessReply(v); return c.Data, err }, func(data []byte) (MaxAccessReply, error) {
			return DecodeMaxAccessReply(CreateContext{Name: "MxAc", Data: data})
		})
		if _, err := DecodeMaxAccessReply(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("FileIDQuery", func(t *testing.T) {
		checkRoundTrip(t, sampleFileIDQuery(), func(v FileIDQuery) ([]byte, error) { c, err := EncodeFileIDQuery(v); return c.Data, err }, func(data []byte) (FileIDQuery, error) {
			return DecodeFileIDQuery(CreateContext{Name: "QFid", Data: data})
		})
		if _, err := DecodeFileIDQuery(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("FileIDReply", func(t *testing.T) {
		checkRoundTrip(t, sampleFileIDReply(), func(v FileIDReply) ([]byte, error) { c, err := EncodeFileIDReply(v); return c.Data, err }, func(data []byte) (FileIDReply, error) {
			return DecodeFileIDReply(CreateContext{Name: "QFid", Data: data})
		})
		if _, err := DecodeFileIDReply(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("DurableRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleDurableRequest(), func(v DurableRequest) ([]byte, error) { c, err := EncodeDurableRequest(v); return c.Data, err }, func(data []byte) (DurableRequest, error) {
			return DecodeDurableRequest(CreateContext{Name: "DH2Q", Data: data})
		})
		if _, err := DecodeDurableRequest(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("DurableReply", func(t *testing.T) {
		checkRoundTrip(t, sampleDurableReply(), func(v DurableReply) ([]byte, error) { c, err := EncodeDurableReply(v); return c.Data, err }, func(data []byte) (DurableReply, error) {
			return DecodeDurableReply(CreateContext{Name: "DH2Q", Data: data})
		})
		if _, err := DecodeDurableReply(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("DurableReconnect", func(t *testing.T) {
		checkRoundTrip(t, sampleDurableReconnect(), func(v DurableReconnect) ([]byte, error) { c, err := EncodeDurableReconnect(v); return c.Data, err }, func(data []byte) (DurableReconnect, error) {
			return DecodeDurableReconnect(CreateContext{Name: "DH2C", Data: data})
		})
		if _, err := DecodeDurableReconnect(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("PreauthContext", func(t *testing.T) {
		checkRoundTrip(t, samplePreauthContext(), func(v PreauthContext) ([]byte, error) { c, err := EncodePreauthContext(v); return c.Data, err }, func(data []byte) (PreauthContext, error) {
			return DecodePreauthContext(NegotiateContext{Type: ContextPreauth, Data: data})
		})
		if _, err := DecodePreauthContext(NegotiateContext{Type: 0xffff}); err == nil {
			t.Fatal("accepted wrong context type")
		}
	})
	t.Run("EncryptionContext", func(t *testing.T) {
		checkRoundTrip(t, sampleEncryptionContext(), func(v EncryptionContext) ([]byte, error) { c, err := EncodeEncryptionContext(v); return c.Data, err }, func(data []byte) (EncryptionContext, error) {
			return DecodeEncryptionContext(NegotiateContext{Type: ContextEncryption, Data: data})
		})
		if _, err := DecodeEncryptionContext(NegotiateContext{Type: 0xffff}); err == nil {
			t.Fatal("accepted wrong context type")
		}
	})
	t.Run("SigningContext", func(t *testing.T) {
		checkRoundTrip(t, sampleSigningContext(), func(v SigningContext) ([]byte, error) { c, err := EncodeSigningContext(v); return c.Data, err }, func(data []byte) (SigningContext, error) {
			return DecodeSigningContext(NegotiateContext{Type: ContextSigning, Data: data})
		})
		if _, err := DecodeSigningContext(NegotiateContext{Type: 0xffff}); err == nil {
			t.Fatal("accepted wrong context type")
		}
	})
	t.Run("AAPLQuery", func(t *testing.T) {
		checkRoundTrip(t, sampleAAPLQuery(), func(v AAPLQuery) ([]byte, error) { c, err := EncodeAAPLQuery(v); return c.Data, err }, func(data []byte) (AAPLQuery, error) { return DecodeAAPLQuery(CreateContext{Name: "AAPL", Data: data}) })
		if _, err := DecodeAAPLQuery(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("AAPLReply", func(t *testing.T) {
		checkRoundTrip(t, sampleAAPLReply(), func(v AAPLReply) ([]byte, error) { c, err := EncodeAAPLReply(v); return c.Data, err }, func(data []byte) (AAPLReply, error) { return DecodeAAPLReply(CreateContext{Name: "AAPL", Data: data}) })
		if _, err := DecodeAAPLReply(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("LeaseContext", func(t *testing.T) {
		checkRoundTrip(t, sampleLeaseContext(), func(v LeaseContext) ([]byte, error) { c, err := EncodeLeaseContext(v); return c.Data, err }, func(data []byte) (LeaseContext, error) {
			return DecodeLeaseContext(CreateContext{Name: "RqLs", Data: data})
		})
		if _, err := DecodeLeaseContext(CreateContext{Name: "wrong"}); err == nil {
			t.Fatal("accepted wrong context tag")
		}
	})
	t.Run("DirectoryIDBothEntries", func(t *testing.T) {
		checkRoundTrip(t, sampleDirectoryIDBothEntries(), EncodeDirectoryIDBothEntries, DecodeDirectoryIDBothEntries)
	})
	t.Run("DirectoryIDFullEntries", func(t *testing.T) {
		checkRoundTrip(t, sampleDirectoryIDFullEntries(), EncodeDirectoryIDFullEntries, DecodeDirectoryIDFullEntries)
	})
	t.Run("Header", func(t *testing.T) { checkRoundTrip(t, sampleHeader(), EncodeHeader, DecodeHeader) })
	t.Run("FileBasicInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileBasicInformation(), EncodeFileBasicInformation, DecodeFileBasicInformation)
	})
	t.Run("FileStandardInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileStandardInformation(), EncodeFileStandardInformation, DecodeFileStandardInformation)
	})
	t.Run("FileInternalInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileInternalInformation(), EncodeFileInternalInformation, DecodeFileInternalInformation)
	})
	t.Run("FileEAInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileEAInformation(), EncodeFileEAInformation, DecodeFileEAInformation)
	})
	t.Run("FileAccessInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileAccessInformation(), EncodeFileAccessInformation, DecodeFileAccessInformation)
	})
	t.Run("FilePositionInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFilePositionInformation(), EncodeFilePositionInformation, DecodeFilePositionInformation)
	})
	t.Run("FileModeInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileModeInformation(), EncodeFileModeInformation, DecodeFileModeInformation)
	})
	t.Run("FileAlignmentInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileAlignmentInformation(), EncodeFileAlignmentInformation, DecodeFileAlignmentInformation)
	})
	t.Run("FileNetworkOpenInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileNetworkOpenInformation(), EncodeFileNetworkOpenInformation, DecodeFileNetworkOpenInformation)
	})
	t.Run("FileAttributeTagInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileAttributeTagInformation(), EncodeFileAttributeTagInformation, DecodeFileAttributeTagInformation)
	})
	t.Run("FileIDInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileIDInformation(), EncodeFileIDInformation, DecodeFileIDInformation)
	})
	t.Run("FileDispositionInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileDispositionInformation(), EncodeFileDispositionInformation, DecodeFileDispositionInformation)
	})
	t.Run("FileEndOfFileInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileEndOfFileInformation(), EncodeFileEndOfFileInformation, DecodeFileEndOfFileInformation)
	})
	t.Run("FileAllocationInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileAllocationInformation(), EncodeFileAllocationInformation, DecodeFileAllocationInformation)
	})
	t.Run("FilesystemSizeInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFilesystemSizeInformation(), EncodeFilesystemSizeInformation, DecodeFilesystemSizeInformation)
	})
	t.Run("FilesystemFullSizeInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFilesystemFullSizeInformation(), EncodeFilesystemFullSizeInformation, DecodeFilesystemFullSizeInformation)
	})
	t.Run("FilesystemDeviceInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFilesystemDeviceInformation(), EncodeFilesystemDeviceInformation, DecodeFilesystemDeviceInformation)
	})
	t.Run("FileNameInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileNameInformation(), EncodeFileNameInformation, DecodeFileNameInformation)
	})
	t.Run("FileAllInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileAllInformation(), EncodeFileAllInformation, DecodeFileAllInformation)
	})
	t.Run("FileRenameInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileRenameInformation(), EncodeFileRenameInformation, DecodeFileRenameInformation)
	})
	t.Run("FilesystemVolumeInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFilesystemVolumeInformation(), EncodeFilesystemVolumeInformation, DecodeFilesystemVolumeInformation)
	})
	t.Run("FilesystemAttributeInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFilesystemAttributeInformation(), EncodeFilesystemAttributeInformation, DecodeFilesystemAttributeInformation)
	})
	t.Run("FileStreamInformation", func(t *testing.T) {
		checkRoundTrip(t, sampleFileStreamInformation(), EncodeFileStreamInformation, DecodeFileStreamInformation)
	})
	t.Run("NegotiateRequest", func(t *testing.T) {
		checkRoundTrip(t, sampleNegotiateRequest(), EncodeNegotiateRequest, func(data []byte) (NegotiateRequest, error) {
			return DecodeNegotiateRequest(Message{Header: Header{Command: Negotiate, Flags: 0}, Body: data})
		})
		checkMessageEnvelope(t, sampleNegotiateRequest(), EncodeNegotiateRequest, DecodeNegotiateRequest, Negotiate, false)
	})
	t.Run("NegotiateResponse", func(t *testing.T) {
		checkRoundTrip(t, sampleNegotiateResponse(), EncodeNegotiateResponse, func(data []byte) (NegotiateResponse, error) {
			return DecodeNegotiateResponse(Message{Header: Header{Command: Negotiate, Flags: FlagResponse}, Body: data})
		})
		checkMessageEnvelope(t, sampleNegotiateResponse(), EncodeNegotiateResponse, DecodeNegotiateResponse, Negotiate, true)
	})
	t.Run("SID", func(t *testing.T) { checkRoundTrip(t, sampleSID(), EncodeSID, DecodeSID) })
	t.Run("ACL", func(t *testing.T) { checkRoundTrip(t, sampleACL(), EncodeACL, DecodeACL) })
	t.Run("SecurityDescriptor", func(t *testing.T) {
		checkRoundTrip(t, sampleSecurityDescriptor(), EncodeSecurityDescriptor, DecodeSecurityDescriptor)
	})
}
