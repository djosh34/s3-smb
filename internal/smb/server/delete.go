package server

import "github.com/djosh34/s3-smb/internal/smb"

// checkDeleteOnClose runs before CREATE can mutate storage. MS-SMB2 3.3.5.9
// requires DELETE in the granted mask for FILE_DELETE_ON_CLOSE.
func checkDeleteOnClose(options, grantedAccess uint32) smb.Status {
	if options&0x00001000 != 0 && grantedAccess&0x00010000 == 0 {
		return smb.StatusAccessDenied
	}
	return smb.StatusSuccess
}
