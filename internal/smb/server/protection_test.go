package server

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// dialProtected logs in with signing only, or with cipher on a server that
// requires encryption.
func dialProtected(t *testing.T, cipher uint16) *testClient {
	t.Helper()
	srv := newTestServer(t)
	if cipher != 0 {
		srv.server.options.Encryption = RequireEncryption
	}
	return srv.dial(t, smbtest.LoginOptions{Cipher: cipher, Signing: smb.SigningGMAC})
}

// A signed session refuses requests not signed with its key, and an encrypted
// session refuses plaintext. The refusal is protected like any reply, and the
// connection goes on.
func TestRefusesUnprotectedRequests(t *testing.T) {
	for _, test := range []struct {
		name      string
		cipher    uint16
		signature [16]byte
	}{
		{"unsigned", 0, [16]byte{}},
		{"wrong signature", 0, [16]byte{1}},
		{"plaintext in an encrypted session", smb.CipherAES128GCM, [16]byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := dialProtected(t, test.cipher)
			header := client.header(wire.Echo, 1)
			if test.signature != ([16]byte{}) {
				header.Flags |= wire.FlagSigned
				header.Signature = test.signature
			}
			client.sendUnprotected(t, wire.Message{Header: header, Body: encode(t, wire.EncodeEchoRequest, wire.EmptyRequest{})})
			if status := client.receive(t, header).Header.Status; status != smb.StatusAccessDenied {
				t.Fatalf("status %#x", status)
			}
			client.echo(t)
		})
	}
}

// An encrypted frame that does not decrypt, or whose members are not all its
// session's own, ends the connection without a reply.
func TestEncryptedFrameErrors(t *testing.T) {
	for name, send := range map[string]func(t *testing.T, client *testClient) error{
		"tag does not verify": func(t *testing.T, client *testClient) error {
			transform := make([]byte, 52+64)
			copy(transform, "\xfdSMB")
			binary.LittleEndian.PutUint32(transform[36:], 64)
			binary.LittleEndian.PutUint16(transform[42:], 1)
			binary.LittleEndian.PutUint64(transform[44:], client.session.SessionID)
			return client.raw.SendRaw(t.Context(), append(binary.BigEndian.AppendUint32(nil, 52+64), transform...))
		},
		"first member related": func(t *testing.T, client *testClient) error {
			first := echoMessage(t, client)
			first.Header.Flags |= wire.FlagRelated
			return client.raw.Send(t.Context(), []wire.Message{first})
		},
		"member of another session": func(t *testing.T, client *testClient) error {
			first, second := echoMessage(t, client), echoMessage(t, client)
			second.Header.SessionID++
			return client.raw.Send(t.Context(), []wire.Message{first, second})
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := dialProtected(t, smb.CipherAES128GCM)
			if err := send(t, client); err != nil {
				t.Fatal(err)
			}
			if reply, err := client.raw.Receive(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("reply %+v, %v", reply.Messages, err)
			}
			if err := client.ended(); err == nil {
				t.Fatal("server ended the connection without an error")
			}
		})
	}
}
