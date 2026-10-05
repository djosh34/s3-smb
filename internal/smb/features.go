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

// NEGOTIATE capabilities. Leasing (0x2) is not advertised, so macOS requests
// neither leases nor durable handles. SMB 3.1.1 negotiates GCM through its
// encryption context, not SMB2_GLOBAL_CAP_ENCRYPTION. No DFS, multichannel,
// persistent handles, directory leases, compression or RDMA.
const (
	CapabilityLargeMTU     uint32 = 0x00000004
	AdvertisedCapabilities        = CapabilityLargeMTU
)

// FileFsAttributeInformation and AAPL masks. Storage is case-sensitive and
// preserves spelling. ACLs, object IDs, sparse files, hard links, open-by-ID,
// quotas, reparse points, named streams and server-side copy are not advertised.
const (
	FileCaseSensitiveSearch        uint32 = 0x00000001
	FileCasePreservedNames         uint32 = 0x00000002
	FileUnicodeOnDisk              uint32 = 0x00000004
	AdvertisedFilesystemAttributes        = FileCaseSensitiveSearch | FileCasePreservedNames | FileUnicodeOnDisk
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
