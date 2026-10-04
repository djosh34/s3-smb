package smbtest

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func protectedClient(t *testing.T, cipher, signing uint16) (*Client, *crypt.Protector) {
	t.Helper()
	options := crypt.Options{SessionKey: bytes.Repeat([]byte{1}, 16), SessionID: 42, Cipher: cipher, Signing: signing, Role: crypt.RoleClient}
	protector, err := crypt.NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	options.Role = crypt.RoleServer
	peer, err := crypt.NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{protector: protector, sessionID: 42, encrypted: cipher != 0, requireEncryption: cipher != 0}, peer
}

func peerPayload(t *testing.T, peer *crypt.Protector, encrypted bool, messages ...wire.Message) []byte {
	t.Helper()
	for index := range messages {
		if !encrypted {
			messages[index].Header.Flags |= wire.FlagSigned
		}
	}
	payload, err := wire.Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted {
		sealed, sealErr := peer.Seal(payload)
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		return sealed
	}
	members, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	for _, member := range members {
		signature, err := peer.Sign(member.Raw)
		if err != nil {
			t.Fatal(err)
		}
		copy(payload[offset+48:offset+64], signature[:])
		offset += len(member.Raw)
	}
	return payload
}

func TestClientProtectsCompoundsWithoutChangingInputs(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
			t.Run(fmt.Sprintf("cipher_%d_signing_%d", cipher, signing), func(t *testing.T) { checkClientProtection(t, cipher, signing) })
		}
	}
}

func checkClientProtection(t *testing.T, cipher, signing uint16) {
	t.Helper()
	client, peer := protectedClient(t, cipher, signing)
	messages := []wire.Message{
		{Header: wire.Header{Command: wire.Echo, MessageID: 7, SessionID: 42, Credit: 9, CreditCharge: 1, Signature: [16]byte{3}}, Body: []byte{4, 0, 0, 0}},
		{Header: wire.Header{Command: wire.Echo, MessageID: 8, SessionID: 42, Credit: 0, CreditCharge: 1}, Body: []byte{4, 0, 0, 0}},
	}
	original := append([]wire.Message(nil), messages...)
	payload, err := client.encodeMessages(messages)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages, original) {
		t.Fatal("protection changed caller messages")
	}
	if cipher != 0 {
		payload, err = peer.Open(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	decoded, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	for index, member := range decoded {
		want := messages[index].Header
		want.NextCommand = member.Header.NextCommand
		want.Signature = member.Header.Signature
		if cipher == 0 {
			want.Flags |= wire.FlagSigned
			if verifyErr := peer.Verify(member.Raw); verifyErr != nil {
				t.Fatal(verifyErr)
			}
		}
		if member.Header != want {
			t.Fatalf("protected header: %+v, want %+v", member.Header, want)
		}
		member.Header.Flags = wire.FlagResponse
		member.Header.Signature = [16]byte{}
		decoded[index] = member
	}
	response := peerPayload(t, peer, cipher != 0, decoded...)
	reply, err := client.decodeMessages(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Messages) != 2 || !bytes.Equal(reply.Raw, response) {
		t.Fatal("protected receive lost members or raw payload")
	}
	response[len(response)-1] ^= 1
	if _, err := client.decodeMessages(response); err == nil {
		t.Fatal("changed response was accepted")
	}
}

func TestClientChecksProtectedReplyIdentity(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		cipher := uint16(0)
		if encrypted {
			cipher = smb.CipherAES128GCM
		}
		client, peer := protectedClient(t, cipher, smb.SigningCMAC)
		for _, header := range []wire.Header{
			{Command: wire.Echo, SessionID: 99, Flags: wire.FlagResponse},
			{Command: wire.Echo, SessionID: 42},
		} {
			payload := peerPayload(t, peer, encrypted, wire.Message{Header: header, Body: []byte{4, 0, 0, 0}})
			if _, err := client.decodeMessages(payload); err == nil {
				t.Fatal("invalid reply identity was accepted")
			}
		}
	}
}

func TestClientAllowsOnlyUnsignedPlaintextInterimReplies(t *testing.T) {
	client, peer := protectedClient(t, 0, smb.SigningGMAC)
	message := wire.Message{Header: wire.Header{Command: wire.Read, MessageID: 7, SessionID: 42, Flags: wire.FlagResponse | wire.FlagAsync, Status: smb.StatusPending, AsyncID: 9}, Body: []byte{9, 0, 0, 0, 0, 0, 0, 0}}
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	if _, decodeErr := client.decodeMessages(payload); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	message.Header.Status = smb.StatusAccessDenied
	payload, err = wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.decodeMessages(payload); err == nil {
		t.Fatal("unsigned final was accepted")
	}
	message.Header.Command = wire.SessionSetup
	message.Header.Status = smb.StatusSuccess
	client.encrypted = true
	if _, err := client.decodeMessages(peerPayload(t, peer, false, message)); err != nil {
		t.Fatal("signed final setup was refused", err)
	}
	message.Header.Command = wire.Echo
	if _, err := client.decodeMessages(peerPayload(t, peer, false, message)); err == nil {
		t.Fatal("encrypted session accepted plaintext")
	}
}

func TestClientRefusesTransformBeforeLogin(t *testing.T) {
	client, peer := protectedClient(t, smb.CipherAES128GCM, smb.SigningCMAC)
	client.protector = nil
	payload := peerPayload(t, peer, true, wire.Message{Header: wire.Header{Command: wire.Echo, SessionID: 42, Flags: wire.FlagResponse}, Body: []byte{4, 0, 0, 0}})
	if _, err := client.decodeMessages(payload); err == nil {
		t.Fatal("transform before login was accepted")
	}
}
