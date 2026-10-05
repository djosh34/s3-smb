package smb

import "time"

// Negotiation policy: SMB 3.1.1 only, with signing required on every session.
// Encrypted traffic is authenticated by GCM and not separately signed.
// Disabling encryption does not disable signing. There is no dialect or cipher
// fallback. Signing uses the MS-SMB2 AES-CMAC default when offers have no overlap.
const (
	DialectWildcard         uint16 = 0x02ff
	Dialect311              uint16 = 0x0311
	PreauthSHA512           uint16 = 0x0001
	SigningCMAC             uint16 = 0x0001
	SigningGMAC             uint16 = 0x0002
	CipherAES128GCM         uint16 = 0x0002
	CipherAES256GCM         uint16 = 0x0004
	SecuritySigningEnabled  uint16 = 0x0001
	SecuritySigningRequired uint16 = 0x0002
	AdvertisedSecurityMode         = SecuritySigningEnabled | SecuritySigningRequired
	SessionEncryptData      uint16 = 0x0004
)

// NEGOTIATE capabilities. macOS asks for leases and durable handles only when
// leasing is advertised. SMB 3.1.1 negotiates GCM through its encryption
// context, not SMB2_GLOBAL_CAP_ENCRYPTION. No DFS, multichannel, persistent
// handles, directory leases, compression or RDMA.
const (
	CapabilityLeasing      uint32 = 0x00000002
	CapabilityLargeMTU     uint32 = 0x00000004
	AdvertisedCapabilities        = CapabilityLeasing | CapabilityLargeMTU
)

// FileFsAttributeInformation and AAPL masks. Storage is case-sensitive,
// preserves spelling and has named streams, which macOS uses for extended
// attributes. ACLs, object IDs, sparse files, hard links, open-by-ID, quotas,
// reparse points and server-side copy are not advertised.
const (
	FileCaseSensitiveSearch        uint32 = 0x00000001
	FileCasePreservedNames         uint32 = 0x00000002
	FileUnicodeOnDisk              uint32 = 0x00000004
	FileNamedStreams               uint32 = 0x00040000
	AdvertisedFilesystemAttributes        = FileCaseSensitiveSearch | FileCasePreservedNames | FileUnicodeOnDisk | FileNamedStreams
	AAPLServerCapabilities         uint64 = 0
	AAPLCaseSensitive              uint64 = 0x00000002
	AAPLFullSync                   uint64 = 0x00000004
	AAPLVolumeCapabilities         uint64 = AAPLCaseSensitive | AAPLFullSync
)

// Feature and resource limits. Durable v2 is only for regular unnamed files
// holding an H lease. Requests above MaxDurableTimeout receive that maximum,
// reported in the reply. A zero request receives DefaultDurableTimeout.
// Durable v1 and persistent contexts receive no grant, and classic oplock
// requests are granted level none.
const (
	LeaseRead             uint32 = 0x01
	LeaseHandle           uint32 = 0x02
	LeaseWrite            uint32 = 0x04
	DefaultDurableTimeout        = 120 * time.Second
	MaxDurableTimeout            = 16 * time.Minute
	MaxStreamSize         uint64 = 64 << 10
	MaxReadSize           uint32 = 1 << 20
	MaxWriteSize          uint32 = 1 << 20
	MaxTransactSize       uint32 = 1 << 20
	CreditUnit            uint32 = 64 << 10
	TargetCredits         uint16 = 256
	MinReconnectCredits   uint16 = 5
	S3OutageWindow               = 5 * time.Minute
)
