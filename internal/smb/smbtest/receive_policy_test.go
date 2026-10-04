package smbtest_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func receivePolicyClient(t *testing.T, cipher, signing uint16, required bool, serve func(net.Conn, *crypt.Protector) error) (*smbtest.Client, smbtest.Session) {
	t.Helper()
	previous := smbtest.Session{SessionID: 42, ClientGUID: [16]byte{11}, Cipher: cipher, Signing: signing}
	account := auth.Account{User: "backup", Password: "password"}
	conn := reconnectPeer(t, func(peer net.Conn) error {
		protector, err := scriptReconnectLogin(peer, previous, account, required)
		if err != nil {
			return err
		}
		return serve(peer, protector)
	})
	client, session, _, err := smbtest.Reconnect(t.Context(), conn, previous, smbtest.LoginOptions{Share: "backup", Account: account}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, session
}

func TestReceiveLeaseBreakEncryptionPolicy(t *testing.T) {
	for _, cipher := range []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
			for _, required := range []bool{false, true} {
				for _, protection := range []string{"unsigned", "signed", "gcm", "tampered gcm"} {
					t.Run(fmt.Sprintf("cipher%d_signing%d_required%t_%s", cipher, signing, required, protection), func(t *testing.T) {
						checkNotificationProtection(t, cipher, signing, required, protection)
					})
				}
			}
		}
	}
}

func checkNotificationProtection(t *testing.T, cipher, signing uint16, required bool, protection string) {
	t.Helper()
	notification, want := breakMessage(t)
	client, _ := receivePolicyClient(t, cipher, signing, required, func(peer net.Conn, protector *crypt.Protector) error {
		if protection == "unsigned" {
			protector = nil
		}
		payload, err := peerEncode(notification, protector, protection == "gcm" || protection == "tampered gcm")
		if err != nil {
			return err
		}
		if protection == "tampered gcm" {
			payload[len(payload)-1] ^= 1
		}
		return writePayload(peer, payload)
	})
	got, err := client.WaitLeaseBreak(t.Context())
	accept := protection == "gcm" || !required && protection == "unsigned"
	if !accept {
		if err == nil {
			t.Fatal("notification without allowed protection accepted")
		}
		return
	}
	if err != nil || got != want {
		t.Fatalf("lease break = %+v, error = %v", got, err)
	}
}

func TestVoluntaryGCMUnsignedNotificationExceptionIsNarrow(t *testing.T) {
	for _, test := range []struct {
		change func(*wire.Message)
		name   string
	}{
		{func(m *wire.Message) { m.Header.Command = wire.Echo }, "command"},
		{func(m *wire.Message) { m.Header.MessageID = 5 }, "message ID"},
		{func(m *wire.Message) { m.Header.SessionID = reconnectSessionID }, "session ID"},
		{func(m *wire.Message) { m.Header.Flags = 0 }, "response flag"},
		{func(m *wire.Message) { m.Header.Flags |= wire.FlagAsync }, "async flag"},
		{func(m *wire.Message) { m.Header.Status = smb.StatusPending }, "status"},
		{func(m *wire.Message) { m.Body = m.Body[:3] }, "short body"},
		{func(m *wire.Message) { m.Body = append(m.Body, 0) }, "extra body"},
		{func(m *wire.Message) { binary.LittleEndian.PutUint16(m.Body, 36) }, "structure size"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, _ := breakMessage(t)
			test.change(&message)
			client, _ := receivePolicyClient(t, smb.CipherAES128GCM, smb.SigningGMAC, false, func(peer net.Conn, _ *crypt.Protector) error {
				payload, err := peerEncode(message, nil, false)
				if err != nil {
					return err
				}
				return writePayload(peer, payload)
			})
			if _, err := client.WaitLeaseBreak(t.Context()); err == nil {
				t.Fatal("invalid unsigned notification accepted")
			}
		})
	}
}

func TestVoluntaryGCMRepliesRequireTransform(t *testing.T) {
	for _, cipher := range []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, required := range []bool{false, true} {
			for _, protection := range []string{"gcm", "tampered gcm", "signed", "unsigned", "unsigned pending"} {
				t.Run(fmt.Sprintf("cipher%d_required%t_%s", cipher, required, protection), func(t *testing.T) {
					checkOrdinaryReplyProtection(t, cipher, required, protection)
				})
			}
		}
	}
}

func checkOrdinaryReplyProtection(t *testing.T, cipher uint16, required bool, protection string) {
	t.Helper()
	client, session := receivePolicyClient(t, cipher, smb.SigningGMAC, required, func(peer net.Conn, protector *crypt.Protector) error {
		return scriptOrdinaryReply(peer, protector, protection)
	})
	message := wire.Message{Header: wire.Header{Command: wire.Echo, MessageID: session.NextMessageID, SessionID: session.SessionID, Credit: 1, CreditCharge: 1}, Body: []byte{4, 0, 0, 0}}
	if sendErr := client.Send(t.Context(), []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	reply, err := client.Receive(t.Context())
	if protection != "gcm" {
		if err == nil {
			t.Fatal("reply without authenticated transform accepted")
		}
		return
	}
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.MessageID != message.Header.MessageID {
		t.Fatalf("encrypted ECHO reply = %+v, error = %v", reply, err)
	}
}

func scriptOrdinaryReply(peer net.Conn, protector *crypt.Protector, protection string) error {
	request, _, err := peerReceive(peer, protector, true)
	if err != nil {
		return err
	}
	if request.Header.Command != wire.Echo || request.Header.Flags&wire.FlagSigned != 0 {
		return errors.New("voluntary request was not an unsigned encrypted ECHO")
	}
	request.Header.Flags = wire.FlagResponse
	if protection == "unsigned" || protection == "unsigned pending" {
		protector = nil
	}
	if protection == "unsigned pending" {
		request.Header.Flags |= wire.FlagAsync
		request.Header.Status = smb.StatusPending
		request.Header.AsyncID = 9
		request.Body, err = wire.EncodeErrorResponse(wire.ErrorResponse{})
		if err != nil {
			return err
		}
	}
	payload, err := peerEncode(request, protector, protection == "gcm" || protection == "tampered gcm")
	if err != nil {
		return err
	}
	if protection == "tampered gcm" {
		payload[len(payload)-1] ^= 1
	}
	return writePayload(peer, payload)
}
