package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLoginAndProtectedEcho(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
			t.Run(fmt.Sprintf("cipher_%d_signing_%d", cipher, signing), func(t *testing.T) {
				options := testOptions(t)
				if cipher == 0 {
					options.Encryption = AllowPlaintext
				}
				server, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				client, ctx, session := loginClient(t, server, cipher, signing)
				if session.SessionID == 0 || session.TreeID == 0 || session.Credits < 5 || session.Cipher != cipher || session.Signing != signing {
					t.Fatalf("login: %+v", session)
				}
				response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID))[0]
				if response.Header.Status != smb.StatusSuccess {
					t.Fatalf("protected echo: %+v", response.Header)
				}
				if cipher == 0 && response.Header.Flags&wire.FlagSigned == 0 {
					t.Fatal("plaintext reply is unsigned")
				}
				if cipher != 0 && response.Header.Flags&wire.FlagSigned != 0 {
					t.Fatal("encrypted reply is separately signed")
				}
			})
		}
	}
}
