package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Reply offsets follow MS-SMB2 2.2.6 (SESSION_SETUP), 2.2.8 (LOGOFF),
// 2.2.10 (TREE_CONNECT) and 2.2.12 (TREE_DISCONNECT). Only requests and the
// prerequisite NEGOTIATE reply use wire codecs.
func TestSessionAndTreeReplyByteLayouts(t *testing.T) {
	for _, mode := range []struct {
		name   string
		policy EncryptionPolicy
	}{
		{"signed_plaintext", AllowPlaintext},
		{"encrypted", RequireEncryption},
	} {
		t.Run(mode.name, func(t *testing.T) {
			options := testOptions(t)
			options.Encryption = mode.policy
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx, sessionID, protector := sessionLayoutLogin(t, server)
			checkTreeAndCleanupLayouts(ctx, t, client, protector, sessionID, mode.policy == RequireEncryption)
		})
	}
}

// This exchange deliberately bypasses Client.Login, which decodes the replies
// under test. Tokens come from offset 72, not from DecodeSessionSetupResponse.
func sessionLayoutLogin(t *testing.T, server *Server) (*smbtest.Client, context.Context, uint64, *crypt.Protector) {
	t.Helper()
	client, ctx := pipeClient(t, server)
	preauth := crypt.NewPreauth()
	request := negotiateMessage(t, 16)
	payload, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	preauth.Update(payload)
	negotiation := exchange(ctx, t, client, request)[0]
	preauth.Update(negotiation.Raw)
	negotiated, err := wire.DecodeNegotiateResponse(negotiation)
	if err != nil {
		t.Fatal(err)
	}
	initiator, err := auth.NewInitiator(server.options.Account, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initiator.Start(negotiated.Token)
	if err != nil {
		t.Fatal(err)
	}
	var sessionID uint64
	var protector *crypt.Protector
	for messageID := uint64(1); messageID <= 2; messageID++ {
		body, encodeErr := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, SecurityMode: 3})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		request = wire.Message{Header: wire.Header{
			Command: wire.SessionSetup, MessageID: messageID, SessionID: sessionID,
			ProcessID: 0x12345678, CreditCharge: 1, Credit: 16,
		}, Body: body}
		payload, err = wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		preauth.Update(payload)
		raw := layoutExchange(ctx, t, client, request)
		if len(raw) <= 72 {
			t.Fatalf("SESSION_SETUP reply length = %d, want a token after offset 72", len(raw))
		}
		want := layoutHeader{
			status: 0xc0000016, flags: layoutResponse, command: 0x0001, charge: 1, credits: 16,
			messageID: messageID, processID: 0x12345678, sessionID: sessionID,
		}
		var sessionFlags uint16
		if messageID == 1 {
			sessionID = binary.LittleEndian.Uint64(raw[40:48])
			if sessionID == 0 || sessionID == ^uint64(0) {
				t.Fatalf("invalid allocated session ID at offset 40: %#x", sessionID)
			}
			want.sessionID = sessionID
			preauth.Update(raw)
		} else {
			want.status, want.flags = 0, layoutResponse|layoutSigned
			if server.options.Encryption == RequireEncryption {
				sessionFlags = 0x0004
			}
			protector, err = crypt.NewProtector(crypt.Options{
				SessionKey: result.SessionKey, Preauth: preauth.Sum(), SessionID: sessionID,
				Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, Role: crypt.RoleClient,
			})
			if err != nil {
				t.Fatal(err)
			}
			if verifyErr := protector.Verify(raw); verifyErr != nil {
				t.Fatalf("final SESSION_SETUP signature: %v", verifyErr)
			}
			// RFC 4178 NegTokenResp, accept-completed, with no response token.
			accepted := []byte{0xa1, 0x07, 0x30, 0x05, 0xa0, 0x03, 0x0a, 0x01, 0x00}
			if !bytes.Equal(raw[72:], accepted) {
				t.Fatalf("final security buffer at offset 72 = %x, want %x", raw[72:], accepted)
			}
		}
		assertLayoutHeader(t, raw, want)
		assertLayoutFields(t, raw,
			layoutField{"SESSION_SETUP structure size", 64, 2, 9},
			layoutField{"session flags", 66, 2, uint64(sessionFlags)},
			layoutField{"security buffer offset", 68, 2, 72},
			layoutField{"security buffer length", 70, 2, uint64(len(raw)) - 72},
		)
		result, err = initiator.Step(raw[72:])
		if err != nil {
			t.Fatalf("SESSION_SETUP security buffer: %v", err)
		}
	}
	if !result.Done {
		t.Fatal("authentication did not complete")
	}
	return client, ctx, sessionID, protector
}

func sessionLayoutProtectedExchange(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector, encrypted bool, request wire.Message) []byte {
	t.Helper()
	var payload []byte
	if encrypted {
		plain, err := wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		payload, err = protector.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		payload = signMessages(t, protector, request)
	}
	sendPayload(ctx, t, client, payload)
	raw := layoutReceive(ctx, t, client)
	if encrypted {
		plain, err := protector.Open(raw)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	if err := protector.Verify(raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func checkTreeAndCleanupLayouts(ctx context.Context, t *testing.T, client *smbtest.Client, protector *crypt.Protector, sessionID uint64, encrypted bool) {
	t.Helper()
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: `\\server\backup`})
	if err != nil {
		t.Fatal(err)
	}
	request := wire.Message{Header: wire.Header{
		Command: wire.TreeConnect, MessageID: 3, SessionID: sessionID,
		ProcessID: 0x76543210, CreditCharge: 1, Credit: 7,
	}, Body: body}
	raw := sessionLayoutProtectedExchange(ctx, t, client, protector, encrypted, request)
	if len(raw) != 80 {
		t.Fatalf("TREE_CONNECT reply length = %d, want 80", len(raw))
	}
	treeID := binary.LittleEndian.Uint32(raw[36:40])
	if treeID == 0 || treeID == ^uint32(0) {
		t.Fatalf("invalid allocated tree ID at offset 36: %#x", treeID)
	}
	flags := uint32(layoutResponse)
	if !encrypted {
		flags |= layoutSigned
	}
	want := layoutHeader{
		flags: flags, command: 0x0003, charge: 1, credits: 7,
		messageID: 3, processID: 0x76543210, sessionID: sessionID, treeID: treeID,
	}
	assertLayoutHeader(t, raw, want)
	assertLayoutFields(t, raw,
		layoutField{"TREE_CONNECT structure size", 64, 2, 16},
		layoutField{"share type (disk)", 66, 1, 0x01},
		layoutField{"TREE_CONNECT reserved", 67, 1, 0},
		layoutField{"share flags", 68, 4, 0},
		layoutField{"share capabilities", 72, 4, 0},
		layoutField{"maximal access", 76, 4, 0x001f01ff},
	)

	body, err = wire.EncodeTreeDisconnectRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Command, request.Header.MessageID, request.Header.TreeID = wire.TreeDisconnect, 4, treeID
	request.Body = body
	raw = sessionLayoutProtectedExchange(ctx, t, client, protector, encrypted, request)
	want.command, want.messageID = 0x0004, 4
	assertLayoutHeader(t, raw, want)
	assertSessionCleanupLayout(t, raw, "TREE_DISCONNECT")

	body, err = wire.EncodeLogoffRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Command, request.Header.MessageID, request.Header.TreeID = wire.Logoff, 5, 0
	request.Body = body
	raw = sessionLayoutProtectedExchange(ctx, t, client, protector, encrypted, request)
	want.command, want.messageID, want.treeID = 0x0002, 5, 0
	assertLayoutHeader(t, raw, want)
	assertSessionCleanupLayout(t, raw, "LOGOFF")
}

func assertSessionCleanupLayout(t *testing.T, raw []byte, command string) {
	t.Helper()
	if len(raw) != 68 {
		t.Fatalf("%s reply length = %d, want 68", command, len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{command + " structure size", 64, 2, 4},
		layoutField{command + " reserved", 66, 2, 0},
	)
}
