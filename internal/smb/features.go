package smb

import "time"

// Negotiation policy from decision #169. Encryption authenticates messages;
// encrypted traffic is not separately signed. Disabling encryption does not
// disable mandatory signing. No dialect or algorithm fallback is permitted.
const (
	DialectWildcard             uint16 = 0x02ff
	Dialect311                  uint16 = 0x0311
	PreauthSHA512               uint16 = 0x0001
	SigningCMAC                 uint16 = 0x0001
	SigningGMAC                 uint16 = 0x0002
	CipherAES128GCM             uint16 = 0x0002
	CipherAES256GCM             uint16 = 0x0004
	SecuritySigningEnabled      uint16 = 0x0001
	SecuritySigningRequired     uint16 = 0x0002
	AdvertisedSecurityMode             = SecuritySigningEnabled | SecuritySigningRequired
	EncryptionRequiredByDefault        = true
	SessionEncryptData          uint16 = 0x0004
	ShareTypeDisk               uint8  = 0x01
	AdvertisedShareCapabilities uint32 = 0
	AdvertisedShareFlags        uint32 = 0
)

// NEGOTIATE advertises only capabilities implemented by the current handlers.
// M5 adds CapabilityLeasing to AdvertisedCapabilities. SMB 3.1.1 negotiates GCM
// only through its encryption context, not SMB2_GLOBAL_CAP_ENCRYPTION. No DFS,
// multichannel, persistent handles, directory leases, compression or RDMA.
const (
	CapabilityLeasing      uint32 = 0x00000002
	CapabilityLargeMTU     uint32 = 0x00000004
	AdvertisedCapabilities        = CapabilityLargeMTU
)

// Exact FileFsAttributeInformation and AAPL masks. Storage is case-sensitive and
// preserves spelling. Do not advertise ACL fidelity, object IDs, sparse files,
// hard links, open-by-ID, quotas, reparse points or server-side copy. M3 omits
// named streams. M4 adds FileNamedStreams to AdvertisedFilesystemAttributes
// and enables AAPLVolumeCapabilities when its AAPL handler is ready.
const (
	FileCaseSensitiveSearch        uint32 = 0x00000001
	FileCasePreservedNames         uint32 = 0x00000002
	FileUnicodeOnDisk              uint32 = 0x00000004
	FileNamedStreams               uint32 = 0x00040000
	AdvertisedFilesystemAttributes        = FileCaseSensitiveSearch | FileCasePreservedNames | FileUnicodeOnDisk
	AAPLServerCapabilities         uint64 = 0
	AAPLCaseSensitive              uint64 = 0x00000002
	AAPLFullSync                   uint64 = 0x00000004
	AAPLVolumeCapabilities         uint64 = 0
)

// Feature and resource limits. Durable v2 is only for regular unnamed files
// holding an H lease. Requests above MaxDurableTimeout receive that maximum,
// reported in the reply. A zero request receives DefaultDurableTimeout.
// Durable v1 and persistent contexts receive no grant.
// Classic oplocks receive level none.
const (
	LeaseVersion          uint16 = 2
	LeaseRead             uint32 = 0x01
	LeaseHandle           uint32 = 0x02
	LeaseWrite            uint32 = 0x04
	OplockNone            uint8  = 0
	DefaultDurableTimeout        = 120 * time.Second
	MaxDurableTimeout            = 16 * time.Minute
	MaxStreamSize         uint64 = 64 << 10
	MaxReadSize           uint32 = 1 << 20
	MaxWriteSize          uint32 = 1 << 20
	MaxTransactSize       uint32 = 1 << 20
	CreditUnit            uint32 = 64 << 10
	TargetCredits         uint16 = 256
	MaxCredits            uint16 = 8192
	MinReconnectCredits   uint16 = 5
	S3OutageWindow               = 5 * time.Minute
)

// Refused feature policy. Unsupported requests receive the command-specific
// status, never a fabricated success. CANCEL has no reply per MS-SMB2; it cannot
// queue a lock or mutate an unrelated async request. IPC$ returns BAD_NETWORK_NAME.
const (
	GuestAllowed               = false
	AnonymousAllowed           = false
	KerberosSupported          = false
	IPCShareSupported          = false
	ShareEnumerationSupported  = false
	DirectoryLeasesSupported   = false
	DurableV1Supported         = false
	PersistentHandlesSupported = false
	BlockingLocksSupported     = false
	ChangeNotifyStatus         = StatusNotSupported
	BonjourSupported           = false
)
