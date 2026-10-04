package wire

import "github.com/djosh34/s3-smb/internal/smb"

func sampleTreeConnectResponse() TreeConnectResponse {
	return TreeConnectResponse{Flags: 1, Capabilities: 2, MaximalAccess: 3, ShareType: 1}
}
func sampleCloseRequest() CloseRequest { return CloseRequest{ID: FileID{1, 2}, Flags: 1} }
func sampleCloseResponse() CloseResponse {
	return CloseResponse{Created: 1, Accessed: 2, Modified: 3, Changed: 4, AllocationSize: 5, Size: 6, Attributes: 7, Flags: 1}
}
func sampleFlushRequest() FlushRequest   { return FlushRequest{ID: FileID{1, 2}, Reserved1: 3} }
func sampleWriteResponse() WriteResponse { return WriteResponse{Count: 1, Remaining: 2} }
func sampleChangeNotifyRequest() ChangeNotifyRequest {
	return ChangeNotifyRequest{ID: FileID{1, 2}, OutputLength: 3, Filter: 4, Flags: 5}
}

func sampleLeaseBreakRequest() LeaseBreakRequest {
	return LeaseBreakRequest{Key: [16]byte{1, 2}, Duration: 3, State: 4, Flags: 5}
}

func sampleLeaseBreakResponse() LeaseBreakResponse {
	return LeaseBreakResponse{Key: [16]byte{1, 2}, Duration: 3, State: 4, Flags: 5}
}

func sampleLeaseBreakNotification() LeaseBreakNotification {
	return LeaseBreakNotification{Key: [16]byte{1, 2}, CurrentState: 3, NewState: 4, Flags: 5, AccessMaskHint: 6, ShareMaskHint: 7, BreakReason: 8, Epoch: 9}
}
func sampleEchoRequest() EmptyRequest             { return EmptyRequest{} }
func sampleEchoResponse() EmptyResponse           { return EmptyResponse{} }
func sampleLogoffRequest() EmptyRequest           { return EmptyRequest{} }
func sampleLogoffResponse() EmptyResponse         { return EmptyResponse{} }
func sampleTreeDisconnectRequest() EmptyRequest   { return EmptyRequest{} }
func sampleTreeDisconnectResponse() EmptyResponse { return EmptyResponse{} }
func sampleCancelRequest() EmptyRequest           { return EmptyRequest{} }
func sampleFlushResponse() EmptyResponse          { return EmptyResponse{} }
func sampleLockResponse() EmptyResponse           { return EmptyResponse{} }
func sampleSetInfoResponse() EmptyResponse        { return EmptyResponse{} }
func sampleSessionSetupRequest() SessionSetupRequest {
	return SessionSetupRequest{Token: []byte{1, 2, 3}, PreviousSessionID: 4, Capabilities: 5, SecurityMode: 3, Flags: 1}
}

func sampleSessionSetupResponse() SessionSetupResponse {
	return SessionSetupResponse{Token: []byte{1, 2, 3}, Flags: 7}
}

func sampleTreeConnectRequest() TreeConnectRequest {
	return TreeConnectRequest{Path: `\\server\share😀`, Flags: 3}
}

func sampleCreateRequest() CreateRequest {
	return CreateRequest{Name: "dir/😀", Contexts: []CreateContext{{Name: "AAPL", Data: []byte{1, 2, 3}}, {Name: "unknown", Data: []byte{4}}}, DesiredAccess: 1, FileAttributes: 2, ShareAccess: 3, Disposition: 4, Options: 5, ImpersonationLevel: 6, OplockLevel: 7}
}

func sampleCreateResponse() CreateResponse {
	return CreateResponse{Contexts: []CreateContext{{Name: "DH2Q", Data: []byte{1, 2}}}, ID: FileID{1, 2}, Created: 3, Accessed: 4, Modified: 5, Changed: 6, AllocationSize: 7, Size: 8, Attributes: 9, Action: 10, Flags: 11, OplockLevel: 12}
}

func sampleReadRequest() ReadRequest {
	return ReadRequest{ChannelInfo: []byte{1, 2}, ID: FileID{3, 4}, Offset: 5, Length: 6, MinimumCount: 7, Channel: 8, RemainingBytes: 9, Flags: 10}
}
func sampleReadResponse() ReadResponse { return ReadResponse{Data: []byte{1, 2, 3}, Remaining: 4} }
func sampleWriteRequest() WriteRequest {
	return WriteRequest{Data: []byte{1, 2, 3}, ChannelInfo: []byte{4, 5}, ID: FileID{6, 7}, Offset: 8, Channel: 9, RemainingBytes: 10, Flags: 11}
}

func sampleLockRequest() LockRequest {
	return LockRequest{Elements: []LockElement{{Offset: 1, Length: 2, Flags: 3}, {Offset: 4, Length: 5, Flags: 6}}, ID: FileID{7, 8}, Sequence: 9}
}

func sampleQueryDirectoryRequest() QueryDirectoryRequest {
	return QueryDirectoryRequest{Pattern: "*.😀", ID: FileID{1, 2}, FileIndex: 3, OutputLength: 4, InfoClass: ClassDirectoryIDBoth, Flags: 5}
}

func sampleQueryInfoRequest() QueryInfoRequest {
	return QueryInfoRequest{Input: []byte{1, 2, 3}, ID: FileID{4, 5}, OutputLength: 6, AdditionalInformation: 7, Flags: 8, InfoType: InfoFile, InfoClass: 4}
}

func sampleSetInfoRequest() SetInfoRequest {
	return SetInfoRequest{Input: []byte{1, 2, 3}, ID: FileID{4, 5}, AdditionalInformation: 6, InfoType: InfoFile, InfoClass: 4}
}
func sampleQueryInfoResponse() QueryResponse      { return QueryResponse{Data: []byte{1, 2, 3}} }
func sampleQueryDirectoryResponse() QueryResponse { return QueryResponse{Data: []byte{1, 2, 3}} }
func sampleIOCTLRequest() IOCTLRequest {
	return IOCTLRequest{Input: []byte{1, 2, 3}, ID: FileID{4, 5}, ControlCode: 6, MaxOutput: 7, Flags: 8}
}

func sampleIOCTLResponse() IOCTLResponse {
	return IOCTLResponse{Input: []byte{1, 2, 3}, Output: []byte{4, 5}, ID: FileID{6, 7}, ControlCode: 8, Flags: 9}
}

func sampleErrorResponse() ErrorResponse {
	return ErrorResponse{Data: []byte{3, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3}, ContextCount: 1}
}
func sampleMaxAccessQuery() MaxAccessQuery { return MaxAccessQuery{Timestamp: 1} }
func sampleMaxAccessReply() MaxAccessReply {
	return MaxAccessReply{Status: smb.Status(0xc0000001), Access: 2}
}
func sampleFileIDQuery() FileIDQuery { return FileIDQuery{} }
func sampleFileIDReply() FileIDReply { return FileIDReply{DiskFileID: 1, VolumeID: 2} }
func sampleDurableRequest() DurableRequest {
	return DurableRequest{CreateGUID: [16]byte{1, 2}, Timeout: 3, Flags: 4}
}
func sampleDurableReply() DurableReply { return DurableReply{Timeout: 1, Flags: 2} }
func sampleDurableReconnect() DurableReconnect {
	return DurableReconnect{ID: FileID{1, 2}, CreateGUID: [16]byte{3, 4}, Flags: 5}
}

func samplePreauthContext() PreauthContext {
	return PreauthContext{Hashes: []uint16{1, 2}, Salt: []byte{3, 4, 5}}
}

func sampleEncryptionContext() EncryptionContext { return EncryptionContext{Ciphers: []uint16{2, 4}} }

func sampleSigningContext() SigningContext { return SigningContext{Algorithms: []uint16{1, 2}} }

func sampleAAPLQuery() AAPLQuery { return AAPLQuery{Requested: 7, ClientCapabilities: 3} }

func sampleAAPLReply() AAPLReply {
	return AAPLReply{Model: "s3-smb😀", Returned: 7, ServerCapabilities: 1, VolumeCapabilities: 6}
}

func sampleLeaseContext() LeaseContext {
	return LeaseContext{Key: [16]byte{1, 2}, ParentKey: [16]byte{3, 4}, Duration: 5, State: 6, Flags: 7, Epoch: 8, Version: 2}
}

func sampleDirectoryIDBothEntries() []DirectoryIDBothEntry {
	return []DirectoryIDBothEntry{{Name: "one😀", ShortName: "ONE", Metadata: DirectoryMetadata{Basic: FileBasicInformation{1, 2, 3, 4, 5}, EndOfFile: 6, AllocationSize: 7, FileID: 8, FileIndex: 9, EASize: 10}}, {Name: "two", ShortName: "TWO", Metadata: DirectoryMetadata{FileID: 11}}}
}

func sampleDirectoryIDFullEntries() []DirectoryIDFullEntry {
	return []DirectoryIDFullEntry{{Name: "one😀", Metadata: DirectoryMetadata{Basic: FileBasicInformation{1, 2, 3, 4, 5}, EndOfFile: 6, AllocationSize: 7, FileID: 8, FileIndex: 9, EASize: 10}}, {Name: "two", Metadata: DirectoryMetadata{FileID: 11}}}
}

func sampleHeader() Header {
	return Header{Command: Write, MessageID: 1, SessionID: 2, TreeID: 3, ProcessID: 4, Credit: 5, CreditCharge: 6, ChannelSequence: 7, Signature: [16]byte{8, 9}}
}

func sampleFileBasicInformation() FileBasicInformation {
	return FileBasicInformation{Created: 1, Accessed: FiletimeSuppress, Modified: FiletimeResume, Changed: 4, Attributes: 5}
}

func sampleFileStandardInformation() FileStandardInformation {
	return FileStandardInformation{AllocationSize: 1, EndOfFile: 2, Links: 3, DeletePending: true, Directory: true}
}

func sampleFileInternalInformation() FileInternalInformation {
	return FileInternalInformation{Index: 1}
}
func sampleFileEAInformation() FileEAInformation         { return FileEAInformation{Size: 2} }
func sampleFileAccessInformation() FileAccessInformation { return FileAccessInformation{Access: 3} }
func sampleFilePositionInformation() FilePositionInformation {
	return FilePositionInformation{Offset: 4}
}
func sampleFileModeInformation() FileModeInformation { return FileModeInformation{Mode: 5} }
func sampleFileAlignmentInformation() FileAlignmentInformation {
	return FileAlignmentInformation{Requirement: 6}
}

func sampleFileNetworkOpenInformation() FileNetworkOpenInformation {
	return FileNetworkOpenInformation{Created: 1, Accessed: 2, Modified: 3, Changed: 4, AllocationSize: 5, EndOfFile: 6, Attributes: 7}
}

func sampleFileAttributeTagInformation() FileAttributeTagInformation {
	return FileAttributeTagInformation{Attributes: 1, Tag: 2}
}

func sampleFileIDInformation() FileIDInformation {
	return FileIDInformation{VolumeSerial: 1, ID: [16]byte{2, 3}}
}

func sampleFileDispositionInformation() FileDispositionInformation {
	return FileDispositionInformation{DeletePending: true}
}

func sampleFileEndOfFileInformation() FileEndOfFileInformation {
	return FileEndOfFileInformation{EndOfFile: 1}
}

func sampleFileAllocationInformation() FileAllocationInformation {
	return FileAllocationInformation{AllocationSize: 2}
}

func sampleFilesystemSizeInformation() FilesystemSizeInformation {
	return FilesystemSizeInformation{TotalUnits: 1, AvailableUnits: 2, SectorsPerUnit: 3, BytesPerSector: 4}
}

func sampleFilesystemFullSizeInformation() FilesystemFullSizeInformation {
	return FilesystemFullSizeInformation{TotalUnits: 1, CallerAvailableUnits: 2, ActualAvailableUnits: 3, SectorsPerUnit: 4, BytesPerSector: 5}
}

func sampleFilesystemDeviceInformation() FilesystemDeviceInformation {
	return FilesystemDeviceInformation{Type: 1, Characteristics: 2}
}

func sampleFileNameInformation() FileNameInformation { return FileNameInformation{Name: "file😀"} }

func sampleFileAllInformation() FileAllInformation {
	return FileAllInformation{Name: FileNameInformation{Name: "name😀"}, Basic: FileBasicInformation{1, 2, 3, 4, 5}, Standard: FileStandardInformation{6, 7, 8, true, true}, Internal: FileInternalInformation{9}, EA: FileEAInformation{10}, Access: FileAccessInformation{11}, Position: FilePositionInformation{12}, Mode: FileModeInformation{13}, Alignment: FileAlignmentInformation{14}}
}

func sampleFileRenameInformation() FileRenameInformation {
	return FileRenameInformation{Name: "new😀", RootDirectory: 1, ReplaceIfExists: true}
}

func sampleFilesystemVolumeInformation() FilesystemVolumeInformation {
	return FilesystemVolumeInformation{Label: "volume😀", Created: 1, Serial: 2, SupportsObjects: true}
}

func sampleFilesystemAttributeInformation() FilesystemAttributeInformation {
	return FilesystemAttributeInformation{Name: "fs😀", Attributes: 1, MaxComponentLength: 255}
}

func sampleFileStreamInformation() FileStreamInformation {
	return FileStreamInformation{Entries: []FileStreamEntry{{Name: "::$DATA", Size: 1, AllocationSize: 2}, {Name: ":😀:$DATA", Size: 3, AllocationSize: 4}}}
}

func sampleNegotiateRequest() NegotiateRequest {
	return NegotiateRequest{Dialects: []uint16{0x311, 0x210}, Contexts: []NegotiateContext{{Type: 1, Data: []byte{1, 0, 2, 0, 1, 0, 3, 4}}, {Type: 0xffff, Data: []byte{5}}}, ClientGUID: [16]byte{1, 2}, Capabilities: 3, SecurityMode: 1}
}

func sampleNegotiateResponse() NegotiateResponse {
	return NegotiateResponse{Token: []byte{1, 2, 3}, Contexts: []NegotiateContext{{Type: 8, Data: []byte{1, 0, 2, 0}}}, ServerGUID: [16]byte{4, 5}, SystemTime: 6, ServerStartTime: 7, Capabilities: 8, MaxTransact: 9, MaxRead: 10, MaxWrite: 11, Dialect: 0x311, SecurityMode: 3}
}

func sampleSID() SID {
	return SID{Revision: 1, Authority: [6]byte{0, 0, 0, 0, 0, 5}, SubAuthorities: []uint32{21, 1, 2, 3}}
}

func sampleACL() ACL {
	return ACL{Revision: 2, Entries: []ACE{{Type: ACEAllowed, Flags: 1, Mask: 2, Trustee: SID{Revision: 1, Authority: [6]byte{0, 0, 0, 0, 0, 5}, SubAuthorities: []uint32{21}}}, {Type: ACEDenied, Flags: 3, Mask: 4, Trustee: SID{Revision: 1}}}}
}

func sampleSecurityDescriptor() SecurityDescriptor {
	return SecurityDescriptor{Revision: 1, Control: DescriptorSelfRelative | DACLPresent | SACLPresent, Owner: &SID{Revision: 1, SubAuthorities: []uint32{1}}, Group: &SID{Revision: 1, SubAuthorities: []uint32{2}}, DACL: &ACL{Revision: 2, Entries: []ACE{{Type: ACEAllowed, Mask: 3, Trustee: SID{Revision: 1}}}}, SACL: &ACL{Revision: 2}}
}
