package server

// MS-FSA 2.1.5.14.2 limits client-settable attributes to these seven bits.
const clientSettableFileAttributes uint32 = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000020 | 0x00000100 | 0x00001000 | 0x00002000

func normalizeFileAttributes(attributes uint32) uint32 {
	attributes &= clientSettableFileAttributes
	if attributes == 0 {
		return 0x80 // FILE_ATTRIBUTE_NORMAL.
	}
	return attributes
}
