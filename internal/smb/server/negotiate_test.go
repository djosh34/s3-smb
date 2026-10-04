package server

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestNegotiateSelects311AndOfferedAlgorithms(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	messages := exchange(ctx, t, client, negotiateMessage(t, 16))
	if len(messages) != 1 || messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("negotiate: %+v", messages)
	}
	response, err := wire.DecodeNegotiateResponse(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if response.Dialect != smb.Dialect311 || response.SecurityMode != smb.AdvertisedSecurityMode || response.Capabilities != smb.CapabilityLargeMTU || response.MaxRead != smb.MaxReadSize || response.MaxWrite != smb.MaxWriteSize || response.MaxTransact != smb.MaxTransactSize || len(response.Token) == 0 {
		t.Fatalf("negotiate response: %+v", response)
	}
	if len(response.Contexts) != 3 {
		t.Fatalf("contexts: %+v", response.Contexts)
	}
	preauth, err := wire.DecodePreauthContext(response.Contexts[0])
	if err != nil || len(preauth.Hashes) != 1 || preauth.Hashes[0] != smb.PreauthSHA512 || len(preauth.Salt) != 32 {
		t.Fatalf("preauth: %+v, %v", preauth, err)
	}
	encryption, err := wire.DecodeEncryptionContext(response.Contexts[1])
	if err != nil || len(encryption.Ciphers) != 1 || encryption.Ciphers[0] != smb.CipherAES256GCM {
		t.Fatalf("encryption: %+v, %v", encryption, err)
	}
	signing, err := wire.DecodeSigningContext(response.Contexts[2])
	if err != nil || len(signing.Algorithms) != 1 || signing.Algorithms[0] != smb.SigningGMAC {
		t.Fatalf("signing: %+v, %v", signing, err)
	}
}

func TestOpeningSMB1NegotiateGetsWildcard(t *testing.T) {
	options := testOptions(t)
	options.Now = func() time.Time { return time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC) }
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	payload := make([]byte, 35)
	copy(payload, []byte{0xff, 'S', 'M', 'B', 0x72})
	dialects := []byte("\x02NT LM 0.12\x00\x02SMB 2.???\x00")
	binary.LittleEndian.PutUint16(payload[33:], 23)
	payload = append(payload, dialects...)
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, 58)
	if sendErr := client.SendRaw(ctx, append(frame, payload...)); sendErr != nil {
		t.Fatal(sendErr)
	}
	reply, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	response, err := wire.DecodeNegotiateResponse(reply.Messages[0])
	if err != nil || response.Dialect != smb.DialectWildcard {
		t.Fatalf("wildcard response: %+v, %v", response, err)
	}
	if reply.Messages[0].Header.Credit != 1 {
		t.Fatal("wildcard must grant one credit")
	}
	assertNegotiateFields(t, response, options)
	// The wildcard consumes MessageId 0. The SMB2 exchange starts at 1.
	request := negotiateMessage(t, 1)
	request.Header.MessageID = 1
	messages := exchange(ctx, t, client, request)
	if messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("SMB2 after wildcard: %+v", messages[0].Header)
	}
	negotiated, err := wire.DecodeNegotiateResponse(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	assertNegotiateFields(t, negotiated, options)
	if messages[0].Header.Credit != 1 {
		t.Fatal("SMB2 NEGOTIATE lost its credit grant")
	}
	if response := exchange(ctx, t, client, echo(t, 2)); response[0].Header.Status != smb.StatusSuccess {
		t.Fatal("wildcard left an invalid credit window")
	}
	requestBytes, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	first := sha512.Sum512(append(make([]byte, 64), requestBytes...))
	expected := sha512.Sum512(append(first[:], messages[0].Raw...))
	// The connection transcript is consumed by SESSION_SETUP in the next PR.
	// Read it after receiving the complete NEGOTIATE response.
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.connections) != 1 {
		t.Fatal("negotiation closed the connection")
	}
	for connection := range server.connections {
		if got := connection.preauth.Sum(); got != expected {
			t.Fatal("preauth transcript includes the wildcard exchange")
		}
	}
}

func TestNegotiateRefusalsLogReason(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		dialect      uint16
		cipher       uint16
		plaintext    bool
		want         smb.Status
	}{
		{name: "no 311", reason: "SMB 3.1.1", dialect: 0x0302, want: smb.StatusNotSupported},
		{name: "no GCM", reason: "AES-GCM", dialect: smb.Dialect311, cipher: 1, want: smb.StatusNotSupported},
		{name: "plaintext permits no GCM", dialect: smb.Dialect311, cipher: 1, plaintext: true, want: smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(t)
			var logs bytes.Buffer
			options.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			if test.plaintext {
				options.Encryption = AllowPlaintext
			}
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			message := negotiateMessage(t, 1)
			request, err := wire.DecodeNegotiateRequest(message)
			if err != nil {
				t.Fatal(err)
			}
			request.Dialects = []uint16{test.dialect}
			if test.dialect != smb.Dialect311 {
				request.Contexts = nil
			} else {
				request.Contexts[1], err = wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: []uint16{test.cipher}})
				if err != nil {
					t.Fatal(err)
				}
			}
			message.Body, err = wire.EncodeNegotiateRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			messages := exchange(ctx, t, client, message)
			if messages[0].Header.Status != test.want {
				t.Fatalf("status: %#x", messages[0].Header.Status)
			}
			if test.reason != "" && !strings.Contains(logs.String(), test.reason) {
				t.Fatalf("missing refusal reason: %s", logs.String())
			}
		})
	}
}
