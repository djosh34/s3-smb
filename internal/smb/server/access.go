package server

// SMB file access masks are expanded before they enter the open table.
const (
	fileReadData   uint32 = 0x00000001
	fileWriteData  uint32 = 0x00000002
	fileAppendData uint32 = 0x00000004
	fileDelete     uint32 = 0x00010000
	fileAllAccess  uint32 = 0x001f01ff
)
