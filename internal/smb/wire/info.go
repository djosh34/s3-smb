package wire

import "time"

// InfoType selects the QUERY_INFO or SET_INFO namespace. Quota is not supported.
type InfoType uint8

// Information namespaces from MS-SMB2.
const (
	InfoFile       InfoType = 1
	InfoFilesystem InfoType = 2
	InfoSecurity   InfoType = 3
)

// FileInfoClass identifies the file classes implemented by M1 codecs.
// Supporting a codec does not advertise that a server handler is ready.
type FileInfoClass uint8

// File class numbers from MS-FSCC.
const (
	ClassFileBasic        FileInfoClass = 4
	ClassFileStandard     FileInfoClass = 5
	ClassFileInternal     FileInfoClass = 6
	ClassFileEA           FileInfoClass = 7
	ClassFileAccess       FileInfoClass = 8
	ClassFileName         FileInfoClass = 9
	ClassFileRename       FileInfoClass = 10
	ClassFileDisposition  FileInfoClass = 13
	ClassFilePosition     FileInfoClass = 14
	ClassFileMode         FileInfoClass = 16
	ClassFileAlignment    FileInfoClass = 17
	ClassFileAll          FileInfoClass = 18
	ClassFileAllocation   FileInfoClass = 19
	ClassFileEndOfFile    FileInfoClass = 20
	ClassFileStream       FileInfoClass = 22
	ClassFileNetworkOpen  FileInfoClass = 34
	ClassFileAttributeTag FileInfoClass = 35
	ClassFileID           FileInfoClass = 59
)

// FilesystemInfoClass identifies supported filesystem class codecs.
type FilesystemInfoClass uint8

// Filesystem class numbers from MS-FSCC.
const (
	ClassFilesystemVolume    FilesystemInfoClass = 1
	ClassFilesystemSize      FilesystemInfoClass = 3
	ClassFilesystemDevice    FilesystemInfoClass = 4
	ClassFilesystemAttribute FilesystemInfoClass = 5
	ClassFilesystemFullSize  FilesystemInfoClass = 7
)

// DirectoryInfoClass identifies supported directory entry layouts.
type DirectoryInfoClass uint8

// Directory class numbers from MS-FSCC.
const (
	ClassDirectoryIDBoth DirectoryInfoClass = 37
	ClassDirectoryIDFull DirectoryInfoClass = 38
)

// Filetime is the raw count of 100-nanosecond ticks since 1601-01-01 UTC.
// Body codecs preserve the bits. SET_INFO interprets sentinels before conversion.
type Filetime uint64

const (
	// FiletimeUnchanged leaves a SET_INFO timestamp unchanged.
	FiletimeUnchanged Filetime = 0
	// FiletimeSuppress is the protocol's -1 sentinel. This server leaves the time
	// unchanged for this SET_INFO only, without suppressing later updates (#189).
	FiletimeSuppress Filetime = 0xffffffffffffffff
	// FiletimeResume is the protocol's -2 sentinel. This server also treats it as
	// unchanged for this SET_INFO only; later I/O updates times as usual (#189).
	FiletimeResume Filetime = 0xfffffffffffffffe
)

// TimeUpdateAction separates an explicit time, including Unix epoch, from a sentinel.
type TimeUpdateAction uint8

// Timestamp actions decoded before any FILETIME conversion.
const (
	TimeKeep TimeUpdateAction = iota
	TimeSet
)

// TimeUpdate is DecodeTimeUpdate's result. Time is valid only for TimeSet.
// TimeKeep leaves the field unchanged for this SET_INFO only. The server keeps
// no per-open suppression state; later I/O updates times as usual (#189).
type TimeUpdate struct {
	Time   time.Time
	Action TimeUpdateAction
}

// FileBasicInformation is shared by QUERY_INFO and SET_INFO. Reserved bytes are
// zero on encode. Times stay raw so SET_INFO cannot overflow a sentinel.
type FileBasicInformation struct {
	Created    Filetime
	Accessed   Filetime
	Modified   Filetime
	Changed    Filetime
	Attributes uint32
}

// FileStandardInformation describes the selected object, not always its base file.
type FileStandardInformation struct {
	AllocationSize uint64
	EndOfFile      uint64
	Links          uint32
	DeletePending  bool
	Directory      bool
}

// FileInternalInformation holds the storage index, not an open's FileID.
type FileInternalInformation struct{ Index uint64 }

// FileEAInformation reports the extended-attribute byte count.
type FileEAInformation struct{ Size uint32 }

// FileAccessInformation returns the full granted SMB mask.
type FileAccessInformation struct{ Access uint32 }

// FilePositionInformation reports the current byte position.
type FilePositionInformation struct{ Offset uint64 }

// FileModeInformation reports the file mode flags.
type FileModeInformation struct{ Mode uint32 }

// FileAlignmentInformation reports the alignment requirement.
type FileAlignmentInformation struct{ Requirement uint32 }

// FileNameInformation carries a UTF-16 name as Go text.
type FileNameInformation struct{ Name string }

// FileAllInformation combines these classes in their MS-FSCC wire order.
// Go field order does not determine wire order; Name precedes fixed data here
// only to keep pointer fields together.
type FileAllInformation struct {
	Name      FileNameInformation
	Basic     FileBasicInformation
	Standard  FileStandardInformation
	Internal  FileInternalInformation
	EA        FileEAInformation
	Access    FileAccessInformation
	Position  FilePositionInformation
	Mode      FileModeInformation
	Alignment FileAlignmentInformation
}

// FileNetworkOpenInformation describes the selected object's times and length.
type FileNetworkOpenInformation struct {
	Created        Filetime
	Accessed       Filetime
	Modified       Filetime
	Changed        Filetime
	AllocationSize uint64
	EndOfFile      uint64
	Attributes     uint32
}

// FileAttributeTagInformation reports file attributes and the reparse tag.
// This server returns a zero tag and does not advertise reparse points.
type FileAttributeTagInformation struct {
	Attributes uint32
	Tag        uint32
}

// FileStreamEntry contains a wire stream name, including the :$DATA suffix.
// The unnamed stream is ::$DATA. The adapter supplies canonical names without it.
type FileStreamEntry struct {
	Name           string
	Size           uint64
	AllocationSize uint64
}

// FileStreamInformation is an offset-linked stream list.
type FileStreamInformation struct{ Entries []FileStreamEntry }

// FileIDInformation reports a stable volume serial and 128-bit storage identity.
type FileIDInformation struct {
	VolumeSerial uint64
	ID           [16]byte
}

// FileDispositionInformation is SET_INFO's deletion flag.
type FileDispositionInformation struct{ DeletePending bool }

// FileEndOfFileInformation is SET_INFO's selected-object length.
type FileEndOfFileInformation struct{ EndOfFile uint64 }

// FileAllocationInformation is SET_INFO's allocation size. Shrinking allocation
// below EOF also shrinks EOF; growing it is only a backend allocation hint.
type FileAllocationInformation struct{ AllocationSize uint64 }

// FileRenameInformation carries SET_INFO's destination name. The handler rejects
// unsupported RootDirectory values. The adapter refuses named-stream rename.
type FileRenameInformation struct {
	Name            string
	RootDirectory   uint64
	ReplaceIfExists bool
}

// FilesystemVolumeInformation describes the volume label and serial.
type FilesystemVolumeInformation struct {
	Label           string
	Created         Filetime
	Serial          uint32
	SupportsObjects bool
}

// FilesystemSizeInformation reports allocation units, not bytes.
type FilesystemSizeInformation struct {
	TotalUnits     uint64
	AvailableUnits uint64
	SectorsPerUnit uint32
	BytesPerSector uint32
}

// FilesystemFullSizeInformation encodes ActualAvailableUnits at byte offset 16.
type FilesystemFullSizeInformation struct {
	TotalUnits           uint64
	CallerAvailableUnits uint64
	ActualAvailableUnits uint64
	SectorsPerUnit       uint32
	BytesPerSector       uint32
}

// FilesystemDeviceInformation describes the volume's device type and flags.
type FilesystemDeviceInformation struct {
	Type            uint32
	Characteristics uint32
}

// FilesystemAttributeInformation carries the exact advertised filesystem mask.
type FilesystemAttributeInformation struct {
	Name               string
	Attributes         uint32
	MaxComponentLength int32
}

// DirectoryMetadata is the fixed data shared by both supported entry layouts.
// Basic.Attributes supplies FileAttributes; the other Basic fields supply times.
type DirectoryMetadata struct {
	Basic          FileBasicInformation
	EndOfFile      uint64
	AllocationSize uint64
	FileID         uint64
	FileIndex      uint32
	EASize         uint32
}

// DirectoryIDBothEntry contains the long and short names for class 37.
// ShortName has at most twelve UTF-16 code units. No AAPL enrichment is advertised.
type DirectoryIDBothEntry struct {
	Name      string
	ShortName string
	Metadata  DirectoryMetadata
}

// DirectoryIDFullEntry is class 38, without the short-name field.
type DirectoryIDFullEntry struct {
	Name     string
	Metadata DirectoryMetadata
}

// SID is a security identifier. Authority is the six bytes in big-endian order;
// SubAuthorities are encoded as little-endian words. Revision must be one.
type SID struct {
	SubAuthorities []uint32
	Authority      [6]byte
	Revision       uint8
}

// ACEType identifies the simple ACE layouts decoded by this server.
type ACEType uint8

// Simple ACE type values from MS-DTYP.
const (
	ACEAllowed ACEType = 0
	ACEDenied  ACEType = 1
	ACEAudit   ACEType = 2
)

// ACE is a simple access or audit entry. Other ACE layouts are unsupported.
type ACE struct {
	Trustee SID
	Mask    uint32
	Flags   uint8
	Type    ACEType
}

// ACL distinguishes an empty list from an absent descriptor ACL.
type ACL struct {
	Entries  []ACE
	Revision uint8
}

// SecurityDescriptor is the self-relative QUERY_INFO and SET_INFO representation.
// Control retains the presence bits. A nil DACL with DACLPresent means a null
// DACL; an empty ACL pointer means an empty DACL. Offsets are codec-owned.
// Decoding this structure does not promise ACL fidelity from the storage adapter.
type SecurityDescriptor struct {
	Owner    *SID
	Group    *SID
	SACL     *ACL
	DACL     *ACL
	Control  uint16
	Revision uint8
}

// Security descriptor control bits needed to preserve absent, null and empty ACLs.
const (
	DACLPresent            uint16 = 0x0004
	SACLPresent            uint16 = 0x0010
	DescriptorSelfRelative uint16 = 0x8000
)
