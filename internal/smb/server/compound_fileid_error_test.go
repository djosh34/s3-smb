package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCompoundWarningAllowsClose(t *testing.T) {
	for _, status := range []smb.Status{smb.StatusBufferOverflow, smb.StatusNoMoreFiles} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%x_async_%t", status, async), func(t *testing.T) {
				checkCompoundWarningClose(t, status, async)
			})
		}
	}
}

func TestCompoundHandlerErrorPreservesFileID(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async_%t", async), func(t *testing.T) {
			checkCompoundHandlerErrorFileID(t, async)
		})
	}
}
