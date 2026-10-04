package smbtest_test

import (
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

func TestVoluntaryGCMReceivesUnsignedLeaseBreak(t *testing.T) {
	previous := smbtest.Session{SessionID: 42, ClientGUID: [16]byte{11}, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC}
	account := auth.Account{User: "backup", Password: "password"}
	notification, want := breakMessage(t)
	conn := reconnectPeer(t, func(peer net.Conn) error {
		if _, err := scriptReconnectLogin(peer, previous, account); err != nil {
			return err
		}
		payload, err := peerEncode(notification, nil, false)
		if err != nil {
			return err
		}
		return writePayload(peer, payload)
	})
	client, _, _, err := smbtest.Reconnect(t.Context(), conn, previous, smbtest.LoginOptions{Share: "backup", Account: account}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	got, err := client.WaitLeaseBreak(t.Context())
	if err != nil || got != want {
		t.Fatalf("voluntary GCM lease break = %+v, error = %v", got, err)
	}
}
