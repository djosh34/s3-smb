package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLateSessionCompletionDuringCleanup(t *testing.T) {
	for _, cleanup := range []struct {
		name    string
		command wire.Command
	}{
		{name: "logoff", command: wire.Logoff},
		{name: "tree_disconnect", command: wire.TreeDisconnect},
	} {
		for _, protection := range []struct {
			name    string
			cipher  uint16
			signing uint16
		}{
			{name: "cmac", signing: smb.SigningCMAC},
			{name: "gmac", signing: smb.SigningGMAC},
			{name: "gcm128", cipher: smb.CipherAES128GCM, signing: smb.SigningGMAC},
			{name: "gcm256", cipher: smb.CipherAES256GCM, signing: smb.SigningGMAC},
		} {
			for _, outcome := range []struct {
				name   string
				status smb.Status
			}{
				{name: "canceled", status: smb.StatusCancelled},
				{name: "late_success", status: smb.StatusSuccess},
				{name: "late_error", status: smb.StatusIODeviceError},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", cleanup.name, protection.name, outcome.name), func(t *testing.T) {
					checkLateSessionCleanup(t, cleanup.command, protection.cipher, protection.signing, outcome.status)
				})
			}
		}
	}
}
