package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLeaseBreakUnsolicitedHeaderAndProtection(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
			t.Run(fmt.Sprintf("cipher_%d_signing_%d", cipher, signing), func(t *testing.T) {
				server, client, ctx, session, _ := leaseServer(t, cipher, signing)
				open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead, false)
				done := startServerBreak(ctx, server, open, 0)
				payload, err := client.ReceiveRaw(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if cipher == 0 {
					checkPlaintextBreak(t, payload)
				} else if !bytes.HasPrefix(payload, []byte{0xfd, 'S', 'M', 'B'}) || len(payload) < 52 || binary.LittleEndian.Uint64(payload[44:52]) != session.SessionID {
					t.Fatalf("notification is not encrypted for the holder session: %x", payload)
				}
				finishServerBreak(ctx, t, done)
			})
		}
	}
}

func checkPlaintextBreak(t *testing.T, payload []byte) {
	t.Helper()
	messages, err := wire.Split(payload)
	if err != nil || len(messages) != 1 {
		t.Fatalf("notification frame: %+v, %v", messages, err)
	}
	header := messages[0].Header
	if header.Command != wire.OplockBreak || header.MessageID != ^uint64(0) || header.SessionID != 0 || header.TreeID != 0 || header.Credit != 0 || header.CreditCharge != 0 || header.Flags != wire.FlagResponse || header.Signature != [16]byte{} {
		t.Fatalf("unsolicited header: %+v", header)
	}
	if _, err := wire.DecodeLeaseBreakNotification(messages[0]); err != nil {
		t.Fatal(err)
	}
}
