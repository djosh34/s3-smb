package wire

// M1 implements these class codecs. M0 records typed signatures without stub
// bodies. Inputs and outputs are class buffers, not SMB headers. Decoders validate
// fixed sizes, lengths, offsets and UTF-16. Encoders write reserved bytes as zero.
// The handler chooses the function from InfoType and InfoClass; unsupported classes
// receive a status instead of being decoded as another class.
//
// Basic is used by both QUERY_INFO and SET_INFO. Its decoder preserves every raw
// Filetime. Other fixed classes below are QUERY_INFO results unless named as a
// SET_INFO class in the next section.
//
//	func DecodeFileBasicInformation(data []byte) (FileBasicInformation, error)
//	func EncodeFileBasicInformation(info FileBasicInformation) ([]byte, error)
//	func DecodeFileStandardInformation(data []byte) (FileStandardInformation, error)
//	func EncodeFileStandardInformation(info FileStandardInformation) ([]byte, error)
//	func DecodeFileInternalInformation(data []byte) (FileInternalInformation, error)
//	func EncodeFileInternalInformation(info FileInternalInformation) ([]byte, error)
//	func DecodeFileEAInformation(data []byte) (FileEAInformation, error)
//	func EncodeFileEAInformation(info FileEAInformation) ([]byte, error)
//	func DecodeFileAccessInformation(data []byte) (FileAccessInformation, error)
//	func EncodeFileAccessInformation(info FileAccessInformation) ([]byte, error)
//	func DecodeFilePositionInformation(data []byte) (FilePositionInformation, error)
//	func EncodeFilePositionInformation(info FilePositionInformation) ([]byte, error)
//	func DecodeFileModeInformation(data []byte) (FileModeInformation, error)
//	func EncodeFileModeInformation(info FileModeInformation) ([]byte, error)
//	func DecodeFileAlignmentInformation(data []byte) (FileAlignmentInformation, error)
//	func EncodeFileAlignmentInformation(info FileAlignmentInformation) ([]byte, error)
//	func DecodeFileNameInformation(data []byte) (FileNameInformation, error)
//	func EncodeFileNameInformation(info FileNameInformation) ([]byte, error)
//	func DecodeFileAllInformation(data []byte) (FileAllInformation, error)
//	func EncodeFileAllInformation(info FileAllInformation) ([]byte, error)
//	func DecodeFileNetworkOpenInformation(data []byte) (FileNetworkOpenInformation, error)
//	func EncodeFileNetworkOpenInformation(info FileNetworkOpenInformation) ([]byte, error)
//	func DecodeFileAttributeTagInformation(data []byte) (FileAttributeTagInformation, error)
//	func EncodeFileAttributeTagInformation(info FileAttributeTagInformation) ([]byte, error)
//	func DecodeFileStreamInformation(data []byte) (FileStreamInformation, error)
//	func EncodeFileStreamInformation(info FileStreamInformation) ([]byte, error)
//	func DecodeFileIDInformation(data []byte) (FileIDInformation, error)
//	func EncodeFileIDInformation(info FileIDInformation) ([]byte, error)
//
// SET_INFO uses Basic above and these concrete mutation classes. Rename preserves
// the destination name for adapter parsing. No decoder mutates storage or state.
//
//	func DecodeFileDispositionInformation(data []byte) (FileDispositionInformation, error)
//	func EncodeFileDispositionInformation(info FileDispositionInformation) ([]byte, error)
//	func DecodeFileEndOfFileInformation(data []byte) (FileEndOfFileInformation, error)
//	func EncodeFileEndOfFileInformation(info FileEndOfFileInformation) ([]byte, error)
//	func DecodeFileAllocationInformation(data []byte) (FileAllocationInformation, error)
//	func EncodeFileAllocationInformation(info FileAllocationInformation) ([]byte, error)
//	func DecodeFileRenameInformation(data []byte) (FileRenameInformation, error)
//	func EncodeFileRenameInformation(info FileRenameInformation) ([]byte, error)
//
// Filesystem codecs preserve allocation units. FullSize's actual available count
// starts at byte offset 16. Volume and Attribute strings use their declared lengths.
//
//	func DecodeFilesystemVolumeInformation(data []byte) (FilesystemVolumeInformation, error)
//	func EncodeFilesystemVolumeInformation(info FilesystemVolumeInformation) ([]byte, error)
//	func DecodeFilesystemSizeInformation(data []byte) (FilesystemSizeInformation, error)
//	func EncodeFilesystemSizeInformation(info FilesystemSizeInformation) ([]byte, error)
//	func DecodeFilesystemFullSizeInformation(data []byte) (FilesystemFullSizeInformation, error)
//	func EncodeFilesystemFullSizeInformation(info FilesystemFullSizeInformation) ([]byte, error)
//	func DecodeFilesystemDeviceInformation(data []byte) (FilesystemDeviceInformation, error)
//	func EncodeFilesystemDeviceInformation(info FilesystemDeviceInformation) ([]byte, error)
//	func DecodeFilesystemAttributeInformation(data []byte) (FilesystemAttributeInformation, error)
//	func EncodeFilesystemAttributeInformation(info FilesystemAttributeInformation) ([]byte, error)
//
// Directory codecs validate the whole offset-linked list before returning entries.
// Encoders calculate NextEntryOffset with eight-byte alignment and zero at the end.
// An empty list encodes as an empty buffer. Names never alias decoder input.
//
//	func DecodeDirectoryIDBothEntries(data []byte) ([]DirectoryIDBothEntry, error)
//	func EncodeDirectoryIDBothEntries(entries []DirectoryIDBothEntry) ([]byte, error)
//	func DecodeDirectoryIDFullEntries(data []byte) ([]DirectoryIDFullEntry, error)
//	func EncodeDirectoryIDFullEntries(entries []DirectoryIDFullEntry) ([]byte, error)
//
// Security codecs support self-relative descriptors and the declared simple ACE
// layouts. They validate SID counts, ACL sizes, descriptor-relative offsets and
// presence bits. Unsupported ACE layouts return an error, never a guessed layout.
//
//	func DecodeSID(data []byte) (SID, error)
//	func EncodeSID(sid SID) ([]byte, error)
//	func DecodeACL(data []byte) (ACL, error)
//	func EncodeACL(acl ACL) ([]byte, error)
//	func DecodeSecurityDescriptor(data []byte) (SecurityDescriptor, error)
//	func EncodeSecurityDescriptor(descriptor SecurityDescriptor) ([]byte, error)
//
// Ordinary Filetime conversion handles range errors without overflowing
// nanoseconds. DecodeFiletime allows zero as the literal 1601 epoch for queries,
// but rejects the -1 and -2 sentinels. SET_INFO instead calls DecodeTimeUpdate
// first: zero, all-ones and all-ones-minus-one return TimeKeep. They leave the
// field unchanged for this SET_INFO only. The server keeps no per-open suppression
// state, and later I/O updates times as usual, per the manager decision on #189.
// EncodeTimeUpdate encodes TimeKeep as zero and rejects a TimeSet value that
// would collide with a sentinel. Basic class codecs still preserve raw sentinel bits.
//
//	func DecodeFiletime(value Filetime) (time.Time, error)
//	func EncodeFiletime(value time.Time) (Filetime, error)
//	func DecodeTimeUpdate(value Filetime) (TimeUpdate, error)
//	func EncodeTimeUpdate(update TimeUpdate) (Filetime, error)
