package smbtest

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func unsignedNotification(t *testing.T) wire.Message {
	t.Helper()
	body, err := wire.EncodeLeaseBreakNotification(wire.LeaseBreakNotification{Key: [16]byte{1}, Epoch: 9, CurrentState: 7, NewState: 3, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.OplockBreak, Flags: wire.FlagResponse, MessageID: ^uint64(0)}, Body: body}
}

func TestUnsignedLeaseBreakOnSignedSession(t *testing.T) {
	for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
		client, _ := protectedClient(t, 0, signing)
		conn, peer := net.Pipe()
		client.conn = conn
		client.pending = make(map[uint64]pendingReply)
		t.Cleanup(func() {
			if err := client.Close(); err != nil {
				t.Error(err)
			}
			if err := peer.Close(); err != nil {
				t.Error(err)
			}
		})
		message := unsignedNotification(t)
		payload, err := wire.Join([]wire.Message{message})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			length := uint32(len(payload) & 0xffffff)
			frame := make([]byte, 4, 4+len(payload))
			binary.BigEndian.PutUint32(frame, length)
			_, writeErr := peer.Write(append(frame, payload...))
			done <- writeErr
		}()
		notification, err := client.WaitLeaseBreak(t.Context())
		if err != nil || notification.Key != [16]byte{1} || notification.Epoch != 9 || notification.NewState != 3 {
			t.Fatalf("unsigned notification = %+v, error = %v", notification, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnsignedLeaseBreakExceptionIsNarrow(t *testing.T) {
	for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
		for _, change := range []func(*wire.Message){
			func(m *wire.Message) { m.Header.Command = wire.Echo },
			func(m *wire.Message) { m.Header.MessageID = 5 },
			func(m *wire.Message) { m.Header.SessionID = 42 },
			func(m *wire.Message) { m.Header.Flags = 0 },
			func(m *wire.Message) { m.Header.Flags |= wire.FlagAsync },
			func(m *wire.Message) { m.Header.Status = smb.StatusAccessDenied },
			func(m *wire.Message) { m.Body = m.Body[:36]; binary.LittleEndian.PutUint16(m.Body, 36) },
			func(m *wire.Message) { m.Body = m.Body[:24]; binary.LittleEndian.PutUint16(m.Body, 24) },
			func(m *wire.Message) { m.Body = m.Body[:3] },
			func(m *wire.Message) { m.Body = append(m.Body, 0) },
			func(m *wire.Message) { binary.LittleEndian.PutUint16(m.Body, 36) },
		} {
			client, _ := protectedClient(t, 0, signing)
			message := unsignedNotification(t)
			change(&message)
			payload, err := wire.Join([]wire.Message{message})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.decodeMessages(payload); err == nil {
				t.Fatal("invalid unsigned notification accepted")
			}
		}
		client, _ := protectedClient(t, 0, signing)
		body, err := wire.EncodeLeaseBreakResponse(wire.LeaseBreakResponse{Key: [16]byte{1}, State: 3})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := wire.Join([]wire.Message{{Header: wire.Header{Command: wire.OplockBreak, Flags: wire.FlagResponse, SessionID: 42, MessageID: 5}, Body: body}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.decodeMessages(payload); err == nil {
			t.Fatal("unsigned acknowledgment reply accepted")
		}
	}
}

func TestEncryptedLeaseBreakStillRequiresGCM(t *testing.T) {
	for _, cipher := range []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM} {
		client, peer := protectedClient(t, cipher, smb.SigningGMAC)
		message := unsignedNotification(t)
		payload, err := wire.Join([]wire.Message{message})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.decodeMessages(payload); err == nil {
			t.Fatal("encrypted session accepted unsigned plaintext notification")
		}
		sealed := peerPayload(t, peer, true, message)
		if _, err := client.decodeMessages(sealed); err != nil {
			t.Fatal(err)
		}
		sealed[len(sealed)-1] ^= 1
		if _, err := client.decodeMessages(sealed); err == nil {
			t.Fatal("encrypted session accepted tampered notification")
		}
	}
}
