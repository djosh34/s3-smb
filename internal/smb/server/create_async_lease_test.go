package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Controlled handlers test transport progress, not lease policy or storage.
func TestPendingCreateAllowsHolderProgress(t *testing.T) {
	for _, command := range []wire.Command{wire.OplockBreak, wire.Close} {
		for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
			t.Run(fmt.Sprintf("command_%d_cipher_%d", command, cipher), func(t *testing.T) {
				checkPendingCreateProgress(t, command, cipher)
			})
		}
	}
}
