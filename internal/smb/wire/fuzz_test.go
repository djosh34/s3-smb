package wire

import (
	"bytes"
	"reflect"
	"testing"
)

// fuzzCodec first checks that value survives a round trip without aliasing
// the decoder input, then fuzzes decode. Any accepted input must re-encode to
// a stable canonical form.
func fuzzCodec[T any](f *testing.F, value T, encode func(T) ([]byte, error), decode func([]byte) (T, error)) {
	f.Helper()
	seed, err := encode(value)
	if err != nil {
		f.Fatal(err)
	}
	input := clone(seed)
	got, err := decode(input)
	if err != nil {
		f.Fatal(err)
	}
	clear(input)
	if !reflect.DeepEqual(got, value) {
		f.Fatalf("round trip: got %+v, want %+v", got, value)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := decode(data)
		if err != nil {
			return
		}
		encoded, err := encode(value)
		if err != nil {
			t.Fatalf("accepted value cannot encode: %v", err)
		}
		again, err := decode(encoded)
		if err != nil {
			t.Fatalf("encoded value cannot decode: %v", err)
		}
		canonical, err := encode(again)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded, canonical) {
			t.Fatal("unstable canonical encoding")
		}
	})
}

func request[T any](decode func(Message) (T, error), command Command) func([]byte) (T, error) {
	return func(data []byte) (T, error) {
		return decode(Message{Header: Header{Command: command}, Body: data})
	}
}

func response[T any](decode func(Message) (T, error), command Command) func([]byte) (T, error) {
	return func(data []byte) (T, error) {
		return decode(Message{Header: Header{Command: command, Flags: FlagResponse}, Body: data})
	}
}

func createContext[T any](encode func(T) (CreateContext, error)) func(T) ([]byte, error) {
	return func(value T) ([]byte, error) {
		c, err := encode(value)
		return c.Data, err
	}
}

func createContextData[T any](decode func(CreateContext) (T, error), name string) func([]byte) (T, error) {
	return func(data []byte) (T, error) { return decode(CreateContext{Name: name, Data: data}) }
}

func negotiateContext[T any](encode func(T) (NegotiateContext, error)) func(T) ([]byte, error) {
	return func(value T) ([]byte, error) {
		c, err := encode(value)
		return c.Data, err
	}
}

func negotiateContextData[T any](decode func(NegotiateContext) (T, error), typ uint16) func([]byte) (T, error) {
	return func(data []byte) (T, error) { return decode(NegotiateContext{Type: typ, Data: data}) }
}

var (
	sampleBasic = FileBasicInformation{Created: 1, Accessed: FiletimeSuppress, Modified: FiletimeResume, Changed: 4, Attributes: 5}
	sampleMeta  = DirectoryMetadata{Basic: FileBasicInformation{1, 2, 3, 4, 5}, EndOfFile: 6, AllocationSize: 7, FileID: 8, FileIndex: 9, EASize: 10}
)

func FuzzDecodeHeader(f *testing.F) {
	fuzzCodec(f, Header{Command: Write, MessageID: 1, SessionID: 2, TreeID: 3, ProcessID: 4, Credit: 5, CreditCharge: 6, ChannelSequence: 7, Signature: [16]byte{8, 9}}, EncodeHeader, DecodeHeader)
}

func FuzzSplit(f *testing.F) {
	packet, err := Join([]Message{{Header: Header{Command: Write, MessageID: 1}, Body: []byte{1, 2, 3}}, {Header: Header{Command: Echo}, Body: []byte{4, 0, 0, 0}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(packet)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, packet []byte) {
		members, err := Split(packet)
		if err != nil {
			return
		}
		joined, err := Join(members)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Split(joined)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(members) {
			t.Fatal("compound member count changed")
		}
		for i, member := range members {
			if again[i].Header != member.Header || !bytes.Equal(again[i].Body, member.Body) {
				t.Fatal("decoded compound changed")
			}
		}
	})
}

func FuzzDecodeSMB1Negotiate(f *testing.F) {
	f.Add(smb1Negotiate("\x02SMB 2.???\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, packet []byte) {
		if DecodeSMB1Negotiate(packet) == nil && len(packet) < 35 {
			t.Fatal("accepted short SMB1 negotiate")
		}
	})
}

func FuzzDecodeFiletime(f *testing.F) {
	for _, v := range []uint64{0, 1, 116444736000000000, 0xffffffffffffffff, 0xfffffffffffffffe, 0xfffffffffffffffd} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, value uint64) {
		decoded, err := DecodeFiletime(Filetime(value))
		if err != nil {
			return
		}
		encoded, err := EncodeFiletime(decoded)
		if err != nil || uint64(encoded) != value {
			t.Fatalf("FILETIME changed: %x, %v", encoded, err)
		}
	})
}

func FuzzDecodeTimeUpdate(f *testing.F) {
	for _, v := range []uint64{0, 1, 116444736000000000, 0xffffffffffffffff, 0xfffffffffffffffe} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, value uint64) {
		decoded, err := DecodeTimeUpdate(Filetime(value))
		if err != nil || decoded.Action == TimeKeep {
			return
		}
		encoded, err := EncodeFiletime(decoded.Time)
		if err != nil || uint64(encoded) != value {
			t.Fatalf("time to set changed: %x, %v", encoded, err)
		}
	})
}

func FuzzDecodeNegotiateRequest(f *testing.F) {
	value := NegotiateRequest{Dialects: []uint16{0x311, 0x210}, Contexts: []NegotiateContext{{Type: 1, Data: []byte{1, 0, 2, 0, 1, 0, 3, 4}}, {Type: 0xffff, Data: []byte{5}}}, ClientGUID: [16]byte{1, 2}, Capabilities: 3, SecurityMode: 1}
	fuzzCodec(f, value, EncodeNegotiateRequest, request(DecodeNegotiateRequest, Negotiate))
}

func FuzzDecodeNegotiateResponse(f *testing.F) {
	value := NegotiateResponse{Token: []byte{1, 2, 3}, Contexts: []NegotiateContext{{Type: 8, Data: []byte{1, 0, 2, 0}}}, ServerGUID: [16]byte{4, 5}, SystemTime: 6, ServerStartTime: 7, Capabilities: 8, MaxTransact: 9, MaxRead: 10, MaxWrite: 11, Dialect: 0x311, SecurityMode: 3}
	fuzzCodec(f, value, EncodeNegotiateResponse, response(DecodeNegotiateResponse, Negotiate))
}

func FuzzDecodeSessionSetupRequest(f *testing.F) {
	value := SessionSetupRequest{Token: []byte{1, 2, 3}, PreviousSessionID: 4, Capabilities: 5, SecurityMode: 3, Flags: 1}
	fuzzCodec(f, value, EncodeSessionSetupRequest, request(DecodeSessionSetupRequest, SessionSetup))
}

func FuzzDecodeSessionSetupResponse(f *testing.F) {
	value := SessionSetupResponse{Token: []byte{1, 2, 3}, Flags: 7}
	fuzzCodec(f, value, EncodeSessionSetupResponse, response(DecodeSessionSetupResponse, SessionSetup))
}

func FuzzDecodeTreeConnectRequest(f *testing.F) {
	value := TreeConnectRequest{Path: `\\server\share😀`, Flags: 3}
	fuzzCodec(f, value, EncodeTreeConnectRequest, request(DecodeTreeConnectRequest, TreeConnect))
}

func FuzzDecodeTreeConnectResponse(f *testing.F) {
	value := TreeConnectResponse{Flags: 1, Capabilities: 2, MaximalAccess: 3, ShareType: 1}
	fuzzCodec(f, value, EncodeTreeConnectResponse, response(DecodeTreeConnectResponse, TreeConnect))
}

func FuzzDecodeCreateRequest(f *testing.F) {
	value := CreateRequest{Name: "dir/😀", Contexts: []CreateContext{{Name: "AAPL", Data: []byte{1, 2, 3}}, {Name: "unknown", Data: []byte{4}}}, DesiredAccess: 1, FileAttributes: 2, ShareAccess: 3, Disposition: 4, Options: 5, ImpersonationLevel: 6, OplockLevel: 7}
	fuzzCodec(f, value, EncodeCreateRequest, request(DecodeCreateRequest, Create))
}

func FuzzDecodeCreateResponse(f *testing.F) {
	value := CreateResponse{Contexts: []CreateContext{{Name: "DH2Q", Data: []byte{1, 2}}}, ID: FileID{1, 2}, Created: 3, Accessed: 4, Modified: 5, Changed: 6, AllocationSize: 7, Size: 8, Attributes: 9, Action: 10, Flags: 11, OplockLevel: 12}
	fuzzCodec(f, value, EncodeCreateResponse, response(DecodeCreateResponse, Create))
}

func FuzzDecodeCloseRequest(f *testing.F) {
	fuzzCodec(f, CloseRequest{ID: FileID{1, 2}, Flags: 1}, EncodeCloseRequest, request(DecodeCloseRequest, Close))
}

func FuzzDecodeCloseResponse(f *testing.F) {
	value := CloseResponse{Created: 1, Accessed: 2, Modified: 3, Changed: 4, AllocationSize: 5, Size: 6, Attributes: 7, Flags: 1}
	fuzzCodec(f, value, EncodeCloseResponse, response(DecodeCloseResponse, Close))
}

func FuzzDecodeFlushRequest(f *testing.F) {
	fuzzCodec(f, FlushRequest{ID: FileID{1, 2}, Reserved1: 3}, EncodeFlushRequest, request(DecodeFlushRequest, Flush))
}

func FuzzDecodeFlushResponse(f *testing.F) {
	fuzzCodec(f, EmptyResponse{}, EncodeFlushResponse, response(DecodeFlushResponse, Flush))
}

func FuzzDecodeReadRequest(f *testing.F) {
	value := ReadRequest{ChannelInfo: []byte{1, 2}, ID: FileID{3, 4}, Offset: 5, Length: 6, MinimumCount: 7, Channel: 8, RemainingBytes: 9, Flags: 10}
	fuzzCodec(f, value, EncodeReadRequest, request(DecodeReadRequest, Read))
}

func FuzzDecodeReadResponse(f *testing.F) {
	fuzzCodec(f, ReadResponse{Data: []byte{1, 2, 3}, Remaining: 4}, EncodeReadResponse, response(DecodeReadResponse, Read))
}

func FuzzDecodeWriteRequest(f *testing.F) {
	value := WriteRequest{Data: []byte{1, 2, 3}, ChannelInfo: []byte{4, 5}, ID: FileID{6, 7}, Offset: 8, Channel: 9, RemainingBytes: 10, Flags: 11}
	fuzzCodec(f, value, EncodeWriteRequest, request(DecodeWriteRequest, Write))
}

func FuzzDecodeWriteResponse(f *testing.F) {
	fuzzCodec(f, WriteResponse{Count: 1, Remaining: 2}, EncodeWriteResponse, response(DecodeWriteResponse, Write))
}

func FuzzDecodeLockRequest(f *testing.F) {
	value := LockRequest{Elements: []LockElement{{Offset: 1, Length: 2, Flags: 3}, {Offset: 4, Length: 5, Flags: 6}}, ID: FileID{7, 8}, Sequence: 9}
	fuzzCodec(f, value, EncodeLockRequest, request(DecodeLockRequest, Lock))
}

func FuzzDecodeLockResponse(f *testing.F) {
	fuzzCodec(f, EmptyResponse{}, EncodeLockResponse, response(DecodeLockResponse, Lock))
}

func FuzzDecodeQueryDirectoryRequest(f *testing.F) {
	value := QueryDirectoryRequest{Pattern: "*.😀", ID: FileID{1, 2}, FileIndex: 3, OutputLength: 4, InfoClass: ClassDirectoryIDBoth, Flags: 5}
	fuzzCodec(f, value, EncodeQueryDirectoryRequest, request(DecodeQueryDirectoryRequest, QueryDirectory))
}

func FuzzDecodeQueryDirectoryResponse(f *testing.F) {
	fuzzCodec(f, QueryResponse{Data: []byte{1, 2, 3}}, EncodeQueryDirectoryResponse, response(DecodeQueryDirectoryResponse, QueryDirectory))
}

func FuzzDecodeQueryInfoRequest(f *testing.F) {
	value := QueryInfoRequest{Input: []byte{1, 2, 3}, ID: FileID{4, 5}, OutputLength: 6, AdditionalInformation: 7, Flags: 8, InfoType: InfoFile, InfoClass: 4}
	fuzzCodec(f, value, EncodeQueryInfoRequest, request(DecodeQueryInfoRequest, QueryInfo))
}

func FuzzDecodeQueryInfoResponse(f *testing.F) {
	fuzzCodec(f, QueryResponse{Data: []byte{1, 2, 3}}, EncodeQueryInfoResponse, response(DecodeQueryInfoResponse, QueryInfo))
}

func FuzzDecodeSetInfoRequest(f *testing.F) {
	value := SetInfoRequest{Input: []byte{1, 2, 3}, ID: FileID{4, 5}, AdditionalInformation: 6, InfoType: InfoFile, InfoClass: 4}
	fuzzCodec(f, value, EncodeSetInfoRequest, request(DecodeSetInfoRequest, SetInfo))
}

func FuzzDecodeSetInfoResponse(f *testing.F) {
	fuzzCodec(f, EmptyResponse{}, EncodeSetInfoResponse, response(DecodeSetInfoResponse, SetInfo))
}

func FuzzDecodeIOCTLRequest(f *testing.F) {
	value := IOCTLRequest{Input: []byte{1, 2, 3}, ID: FileID{4, 5}, ControlCode: 6, MaxOutput: 7, Flags: 8}
	fuzzCodec(f, value, EncodeIOCTLRequest, request(DecodeIOCTLRequest, IOCTL))
}

func FuzzDecodeChangeNotifyRequest(f *testing.F) {
	value := ChangeNotifyRequest{ID: FileID{1, 2}, OutputLength: 3, Filter: 4, Flags: 5}
	fuzzCodec(f, value, EncodeChangeNotifyRequest, request(DecodeChangeNotifyRequest, ChangeNotify))
}

func FuzzDecodeLeaseBreakRequest(f *testing.F) {
	value := LeaseBreakRequest{Key: [16]byte{1, 2}, Duration: 3, State: 4, Flags: 5}
	fuzzCodec(f, value, EncodeLeaseBreakRequest, request(DecodeLeaseBreakRequest, OplockBreak))
}

func FuzzDecodeLeaseBreakResponse(f *testing.F) {
	value := LeaseBreakResponse{Key: [16]byte{1, 2}, Duration: 3, State: 4, Flags: 5}
	fuzzCodec(f, value, EncodeLeaseBreakResponse, response(DecodeLeaseBreakResponse, OplockBreak))
}

func FuzzDecodeLeaseBreakNotification(f *testing.F) {
	value := LeaseBreakNotification{Key: [16]byte{1, 2}, CurrentState: 3, NewState: 4, Flags: 5, AccessMaskHint: 6, ShareMaskHint: 7, BreakReason: 8, Epoch: 9}
	fuzzCodec(f, value, EncodeLeaseBreakNotification, response(DecodeLeaseBreakNotification, OplockBreak))
}

func FuzzDecodeOplockBreakRequest(f *testing.F) {
	value := OplockBreakRequest{ID: FileID{Persistent: 7, Volatile: 8}, Level: 1}
	fuzzCodec(f, value, EncodeOplockBreakRequest, request(DecodeOplockBreakRequest, OplockBreak))
}

func FuzzDecodeEchoRequest(f *testing.F) {
	fuzzCodec(f, EmptyRequest{}, EncodeEchoRequest, request(DecodeEchoRequest, Echo))
}

func FuzzDecodeEchoResponse(f *testing.F) {
	fuzzCodec(f, EmptyResponse{}, EncodeEchoResponse, response(DecodeEchoResponse, Echo))
}

func FuzzDecodeLogoffRequest(f *testing.F) {
	fuzzCodec(f, EmptyRequest{}, EncodeLogoffRequest, request(DecodeLogoffRequest, Logoff))
}

func FuzzDecodeTreeDisconnectRequest(f *testing.F) {
	fuzzCodec(f, EmptyRequest{}, EncodeTreeDisconnectRequest, request(DecodeTreeDisconnectRequest, TreeDisconnect))
}

func FuzzDecodeCancelRequest(f *testing.F) {
	fuzzCodec(f, EmptyRequest{}, EncodeCancelRequest, request(DecodeCancelRequest, Cancel))
}

func FuzzDecodeErrorResponse(f *testing.F) {
	value := ErrorResponse{Data: []byte{3, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3}, ContextCount: 1}
	fuzzCodec(f, value, EncodeErrorResponse, response(DecodeErrorResponse, Echo))
}

func FuzzDecodePreauthContext(f *testing.F) {
	value := PreauthContext{Hashes: []uint16{1, 2}, Salt: []byte{3, 4, 5}}
	fuzzCodec(f, value, negotiateContext(EncodePreauthContext), negotiateContextData(DecodePreauthContext, ContextPreauth))
}

func FuzzDecodeEncryptionContext(f *testing.F) {
	value := EncryptionContext{Ciphers: []uint16{2, 4}}
	fuzzCodec(f, value, negotiateContext(EncodeEncryptionContext), negotiateContextData(DecodeEncryptionContext, ContextEncryption))
}

func FuzzDecodeSigningContext(f *testing.F) {
	value := SigningContext{Algorithms: []uint16{1, 2}}
	fuzzCodec(f, value, negotiateContext(EncodeSigningContext), negotiateContextData(DecodeSigningContext, ContextSigning))
}

func FuzzDecodeAAPLQuery(f *testing.F) {
	value := AAPLQuery{Requested: 7, ClientCapabilities: 3}
	fuzzCodec(f, value, createContext(EncodeAAPLQuery), createContextData(DecodeAAPLQuery, "AAPL"))
}

func FuzzDecodeAAPLReply(f *testing.F) {
	value := AAPLReply{Model: "s3-smb😀", Returned: 7, ServerCapabilities: 1, VolumeCapabilities: 6}
	fuzzCodec(f, value, createContext(EncodeAAPLReply), createContextData(DecodeAAPLReply, "AAPL"))
}

func FuzzDecodeDurableRequest(f *testing.F) {
	value := DurableRequest{CreateGUID: [16]byte{1, 2}, Timeout: 3, Flags: 4}
	fuzzCodec(f, value, createContext(EncodeDurableRequest), createContextData(DecodeDurableRequest, "DH2Q"))
}

func FuzzDecodeDurableReply(f *testing.F) {
	value := DurableReply{Timeout: 1, Flags: 2}
	fuzzCodec(f, value, createContext(EncodeDurableReply), createContextData(DecodeDurableReply, "DH2Q"))
}

func FuzzDecodeDurableReconnect(f *testing.F) {
	value := DurableReconnect{ID: FileID{1, 2}, CreateGUID: [16]byte{3, 4}, Flags: 5}
	fuzzCodec(f, value, createContext(EncodeDurableReconnect), createContextData(DecodeDurableReconnect, "DH2C"))
}

func FuzzDecodeLeaseContext(f *testing.F) {
	value := LeaseContext{Key: [16]byte{1, 2}, ParentKey: [16]byte{3, 4}, Duration: 5, State: 6, Flags: 7, Epoch: 8, Version: 2}
	fuzzCodec(f, value, createContext(EncodeLeaseContext), createContextData(DecodeLeaseContext, "RqLs"))
}

func FuzzDecodeFileBasicInformation(f *testing.F) {
	fuzzCodec(f, sampleBasic, EncodeFileBasicInformation, DecodeFileBasicInformation)
}

func FuzzDecodeFileStandardInformation(f *testing.F) {
	value := FileStandardInformation{AllocationSize: 1, EndOfFile: 2, Links: 3, DeletePending: true, Directory: true}
	fuzzCodec(f, value, EncodeFileStandardInformation, DecodeFileStandardInformation)
}

func FuzzDecodeFileInternalInformation(f *testing.F) {
	fuzzCodec(f, FileInternalInformation{Index: 1}, EncodeFileInternalInformation, DecodeFileInternalInformation)
}

func FuzzDecodeFileEAInformation(f *testing.F) {
	fuzzCodec(f, FileEAInformation{Size: 2}, EncodeFileEAInformation, DecodeFileEAInformation)
}

func FuzzDecodeFileAccessInformation(f *testing.F) {
	fuzzCodec(f, FileAccessInformation{Access: 3}, EncodeFileAccessInformation, DecodeFileAccessInformation)
}

func FuzzDecodeFilePositionInformation(f *testing.F) {
	fuzzCodec(f, FilePositionInformation{Offset: 4}, EncodeFilePositionInformation, DecodeFilePositionInformation)
}

func FuzzDecodeFileModeInformation(f *testing.F) {
	fuzzCodec(f, FileModeInformation{Mode: 5}, EncodeFileModeInformation, DecodeFileModeInformation)
}

func FuzzDecodeFileAlignmentInformation(f *testing.F) {
	fuzzCodec(f, FileAlignmentInformation{Requirement: 6}, EncodeFileAlignmentInformation, DecodeFileAlignmentInformation)
}

func FuzzDecodeFileNameInformation(f *testing.F) {
	fuzzCodec(f, FileNameInformation{Name: "file😀"}, EncodeFileNameInformation, DecodeFileNameInformation)
}

func FuzzDecodeFileAllInformation(f *testing.F) {
	fuzzCodec(f, sampleFileAll, EncodeFileAllInformation, DecodeFileAllInformation)
}

func FuzzDecodeFileNetworkOpenInformation(f *testing.F) {
	value := FileNetworkOpenInformation{Created: 1, Accessed: 2, Modified: 3, Changed: 4, AllocationSize: 5, EndOfFile: 6, Attributes: 7}
	fuzzCodec(f, value, EncodeFileNetworkOpenInformation, DecodeFileNetworkOpenInformation)
}

func FuzzDecodeFileAttributeTagInformation(f *testing.F) {
	value := FileAttributeTagInformation{Attributes: 1, Tag: 2}
	fuzzCodec(f, value, EncodeFileAttributeTagInformation, DecodeFileAttributeTagInformation)
}

func FuzzDecodeFileStreamInformation(f *testing.F) {
	value := FileStreamInformation{Entries: []FileStreamEntry{{Name: "::$DATA", Size: 1, AllocationSize: 2}, {Name: ":😀:$DATA", Size: 3, AllocationSize: 4}}}
	fuzzCodec(f, value, EncodeFileStreamInformation, DecodeFileStreamInformation)
}

func FuzzDecodeFileIDInformation(f *testing.F) {
	fuzzCodec(f, FileIDInformation{VolumeSerial: 1, ID: [16]byte{2, 3}}, EncodeFileIDInformation, DecodeFileIDInformation)
}

func FuzzDecodeFileDispositionInformation(f *testing.F) {
	value := FileDispositionInformation{DeletePending: true}
	fuzzCodec(f, value, EncodeFileDispositionInformation, DecodeFileDispositionInformation)
}

func FuzzDecodeFileEndOfFileInformation(f *testing.F) {
	fuzzCodec(f, FileEndOfFileInformation{EndOfFile: 1}, EncodeFileEndOfFileInformation, DecodeFileEndOfFileInformation)
}

func FuzzDecodeFileAllocationInformation(f *testing.F) {
	value := FileAllocationInformation{AllocationSize: 2}
	fuzzCodec(f, value, EncodeFileAllocationInformation, DecodeFileAllocationInformation)
}

func FuzzDecodeFileRenameInformation(f *testing.F) {
	value := FileRenameInformation{Name: "new😀", RootDirectory: 1, ReplaceIfExists: true}
	fuzzCodec(f, value, EncodeFileRenameInformation, DecodeFileRenameInformation)
}

func FuzzDecodeFilesystemVolumeInformation(f *testing.F) {
	value := FilesystemVolumeInformation{Label: "volume😀", Created: 1, Serial: 2, SupportsObjects: true}
	fuzzCodec(f, value, EncodeFilesystemVolumeInformation, DecodeFilesystemVolumeInformation)
}

func FuzzDecodeFilesystemDeviceInformation(f *testing.F) {
	value := FilesystemDeviceInformation{Type: 1, Characteristics: 2}
	fuzzCodec(f, value, EncodeFilesystemDeviceInformation, DecodeFilesystemDeviceInformation)
}

func FuzzDecodeFilesystemAttributeInformation(f *testing.F) {
	value := FilesystemAttributeInformation{Name: "fs😀", Attributes: 1, MaxComponentLength: -255}
	fuzzCodec(f, value, EncodeFilesystemAttributeInformation, DecodeFilesystemAttributeInformation)
}

func FuzzDecodeDirectoryEntries(f *testing.F) {
	fuzzCodec(f, sampleDirectory(ClassDirectory), EncodeDirectoryEntries, DecodeDirectoryEntries)
}

func FuzzDecodeDirectoryFullEntries(f *testing.F) {
	fuzzCodec(f, sampleDirectory(ClassDirectoryFull), EncodeDirectoryFullEntries, DecodeDirectoryFullEntries)
}

func FuzzDecodeDirectoryBothEntries(f *testing.F) {
	fuzzCodec(f, sampleDirectory(ClassDirectoryBoth), EncodeDirectoryBothEntries, DecodeDirectoryBothEntries)
}

func FuzzDecodeDirectoryNamesEntries(f *testing.F) {
	fuzzCodec(f, sampleDirectory(ClassDirectoryNames), EncodeDirectoryNamesEntries, DecodeDirectoryNamesEntries)
}

func FuzzDecodeDirectoryIDBothEntries(f *testing.F) {
	value := []DirectoryIDBothEntry{{Name: "one😀", ShortName: "ONE", Metadata: sampleMeta}, {Name: "two", ShortName: "TWO", Metadata: DirectoryMetadata{FileID: 11}}}
	fuzzCodec(f, value, EncodeDirectoryIDBothEntries, DecodeDirectoryIDBothEntries)
}

func FuzzDecodeDirectoryIDFullEntries(f *testing.F) {
	value := []DirectoryIDFullEntry{{Name: "one😀", Metadata: sampleMeta}, {Name: "two", Metadata: DirectoryMetadata{FileID: 11}}}
	fuzzCodec(f, value, EncodeDirectoryIDFullEntries, DecodeDirectoryIDFullEntries)
}

var sampleFileAll = FileAllInformation{Name: FileNameInformation{Name: "name😀"}, Basic: FileBasicInformation{1, 2, 3, 4, 5}, Standard: FileStandardInformation{6, 7, 8, true, true}, Internal: FileInternalInformation{9}, EA: FileEAInformation{10}, Access: FileAccessInformation{11}, Position: FilePositionInformation{12}, Mode: FileModeInformation{13}, Alignment: FileAlignmentInformation{14}}

// sampleDirectory fills only the fields present in the class layout, so a
// round trip compares equal.
func sampleDirectory(class DirectoryInfoClass) []DirectoryEntry {
	metadata := DirectoryMetadata{FileIndex: 7}
	if class != ClassDirectoryNames {
		metadata.Basic = FileBasicInformation{Created: 11, Accessed: 12, Modified: 13, Changed: 14, Attributes: 0x20}
		metadata.EndOfFile, metadata.AllocationSize = 101, 4096
	}
	if class == ClassDirectoryFull || class == ClassDirectoryBoth {
		metadata.EASize = 17
	}
	entry := DirectoryEntry{Name: "a😀", Metadata: metadata}
	if class == ClassDirectoryBoth {
		entry.ShortName = "A"
	}
	return []DirectoryEntry{entry, {Name: "second", Metadata: metadata}}
}
