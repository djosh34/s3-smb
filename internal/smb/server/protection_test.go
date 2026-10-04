package server

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// rawLogin hashes the exact handshake independently of crypt.Preauth and the
// client's Login. It leaves subsequent protection under the test's control.
func rawLogin(t *testing.T, server *Server) (*smbtest.Client, context.Context, uint64, *crypt.Protector) {
	t.Helper()
	client, ctx := pipeClient(t, server)
	var transcript crypt.PreauthHash
	update := func(payload []byte) { transcript = sha512.Sum512(append(transcript[:], payload...)) }
	request := negotiateMessage(t, 16)
	raw, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	update(raw)
	response := exchange(ctx, t, client, request)[0]
	update(response.Raw)
	negotiated, err := wire.DecodeNegotiateResponse(response)
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
	id := uint64(0)
	var key []byte
	for messageID := uint64(1); messageID <= 2; messageID++ {
		body, encodeErr := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, SecurityMode: 3})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		request = wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: messageID, SessionID: id, CreditCharge: 1, Credit: 16}, Body: body}
		raw, err = wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		update(raw)
		response = exchange(ctx, t, client, request)[0]
		id = response.Header.SessionID
		if response.Header.Credit < 5 {
			t.Fatal("setup did not grant five credits")
		}
		setup, decodeErr := wire.DecodeSessionSetupResponse(response)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if messageID == 1 {
			if response.Header.Status != smb.StatusMoreProcessingRequired {
				t.Fatalf("challenge: %+v", response.Header)
			}
			update(response.Raw)
		} else if response.Header.Status != smb.StatusSuccess || response.Header.Flags&wire.FlagSigned == 0 {
			t.Fatalf("final setup: %+v", response.Header)
		}
		result, err = initiator.Step(setup.Token)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.SessionKey) != 0 {
			key = result.SessionKey
		}
	}
	options := crypt.Options{SessionKey: key, Preauth: transcript, SessionID: id, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, Role: crypt.RoleClient}
	protector, err := crypt.NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	if verifyErr := protector.Verify(response.Raw); verifyErr != nil {
		t.Fatalf("final signature does not match transcript: %v", verifyErr)
	}
	update(response.Raw)
	options.Preauth = transcript
	wrong, err := crypt.NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.Verify(response.Raw); err == nil {
		t.Fatal("final response was included in key derivation")
	}
	return client, ctx, id, protector
}

func signMessages(t *testing.T, protector *crypt.Protector, messages ...wire.Message) []byte {
	t.Helper()
	for index := range messages {
		messages[index].Header.Flags |= wire.FlagSigned
	}
	payload, err := wire.Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	for _, member := range members {
		signature, err := protector.Sign(member.Raw)
		if err != nil {
			t.Fatal(err)
		}
		copy(payload[offset+48:offset+64], signature[:])
		offset += len(member.Raw)
	}
	return payload
}

func sendPayload(ctx context.Context, t *testing.T, client *smbtest.Client, payload []byte) {
	t.Helper()
	frame := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	if err := client.SendRaw(ctx, append(frame, payload...)); err != nil {
		t.Fatal(err)
	}
}

func TestFinalSessionSetupMatchesTranscript(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	rawLogin(t, server)
}

func TestPlaintextCompoundVerifiesEveryMemberBeforeDispatch(t *testing.T) {
	for _, tamper := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("member_%d", tamper), func(t *testing.T) { checkPlainCompound(t, tamper) })
	}
}

func checkPlainCompound(t *testing.T, tamper int) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		calls.Add(1)
		return handleEcho(ctx, request, message)
	}
	client, ctx, id, protector := rawLogin(t, server)
	first, second := echo(t, 3), echo(t, 4)
	first.Header.SessionID = id
	second.Header.SessionID, second.Header.Flags = ^uint64(0), wire.FlagRelated
	payload := signMessages(t, protector, first, second)
	if tamper >= 0 {
		payload[tamper*72+48] ^= 1
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.Receive(ctx)
	if tamper >= 0 {
		if err == nil || calls.Load() != 0 {
			t.Fatal("tampered compound reached a handler")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 2 || calls.Load() != 2 {
		t.Fatalf("compound: %+v", response.Messages)
	}
	for _, member := range response.Messages {
		if member.Header.SessionID != id || member.Header.Status != smb.StatusSuccess {
			t.Fatalf("related identity: %+v", member.Header)
		}
		if verifyErr := protector.Verify(member.Raw); verifyErr != nil {
			t.Fatal(verifyErr)
		}
	}
}

func TestEncryptionRejectsChangedTagAndPlaintextBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"tag", "ciphertext", "session", "plaintext", "valid"} {
		t.Run(mode, func(t *testing.T) { checkEncryptionInput(t, mode) })
	}
}

func checkEncryptionInput(t *testing.T, mode string) {
	t.Helper()
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		calls.Add(1)
		return handleEcho(ctx, request, message)
	}
	client, ctx, id, protector := rawLogin(t, server)
	message := echo(t, 3)
	message.Header.SessionID = id
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	if mode == "plaintext" {
		payload = signMessages(t, protector, message)
	} else {
		payload, err = protector.Seal(payload)
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "tag":
			payload[4] ^= 1
		case "ciphertext":
			payload[len(payload)-1] ^= 1
		case "session":
			payload[44] ^= 1
		}
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.ReceiveRaw(ctx)
	if mode != "valid" {
		if err == nil || calls.Load() != 0 {
			t.Fatal("invalid protection reached a handler")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(plain)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || members[0].Header.Flags&wire.FlagSigned != 0 {
		t.Fatal("encrypted response was signed separately")
	}
}
