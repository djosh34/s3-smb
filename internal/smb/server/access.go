package server

// SMB file access masks are expanded before they enter the open table.
const (
	fileReadData   uint32 = 0x00000001
	fileWriteData  uint32 = 0x00000002
	fileAppendData uint32 = 0x00000004
	fileDelete     uint32 = 0x00010000
	fileAllAccess  uint32 = 0x001f01ff

	fileGenericRead    uint32 = 0x00120089
	fileGenericWrite   uint32 = 0x00120116
	fileGenericExecute uint32 = 0x001200a0
	maximumAllowed     uint32 = 0x02000000
	genericAll         uint32 = 0x10000000
	genericExecute     uint32 = 0x20000000
	genericWrite       uint32 = 0x40000000
	genericRead        uint32 = 0x80000000
)
