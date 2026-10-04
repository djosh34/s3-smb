package server

import (
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReplacementBetweenVerificationAndDispatchRetainsReplyProtection(t *testing.T) {
	for _, test := range []struct {
		name      string
		encrypted bool
		required  bool
		denied    bool
	}{
		{name: "signed"},
		{name: "encrypted", encrypted: true, required: true},
		{name: "encrypted_when_plaintext_allowed", encrypted: true},
		{name: "bad_signature_denial", denied: true},
		{name: "plaintext_policy_denial", required: true, denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkProtectionHandoff(t, test.encrypted, test.required, test.denied)
		})
	}
}

func checkProtectionHandoff(t *testing.T, encrypted, required, denied bool) {
	t.Helper()
	options := testOptions(t)
	options.Encryption = AllowPlaintext
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = 99
	protection := crypt.Options{SessionKey: []byte("0123456789abcdef"), SessionID: sessionID, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC}
	serverKey, err := crypt.NewProtector(protection)
	if err != nil {
		t.Fatal(err)
	}
	protection.Role = crypt.RoleClient
	clientKey, err := crypt.NewProtector(protection)
	if err != nil {
		t.Fatal(err)
	}
	owner := newConnection(t.Context(), func() {}, server, nil)
	owner.sessions[sessionID] = &sessionEntry{active: true, protector: serverKey, identity: Session{SessionID: sessionID, User: options.Account.User, Encrypted: required}}
	server.registerSession(sessionID, owner)
	requests := []wire.Message{sessionEcho(t, smbtest.Session{SessionID: sessionID}, 1), sessionEcho(t, smbtest.Session{SessionID: sessionID}, 2)}
	payload, err := wire.Join(requests)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted {
		payload, err = clientKey.Seal(payload)
	} else {
		payload = signMessages(t, clientKey, requests...)
		if denied && !required {
			payload[48] ^= 1
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	messages, err := owner.decodePayload(payload)
	if denied && !errors.Is(err, errAccessDenied) || !denied && err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Complete a replacement on another connection at the old receive loop's
	// verification-to-retention handoff, before any member is dispatched.
	client, ctx := pipeClient(t, server)
	fresh, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}, PreviousSessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionID == sessionID {
		t.Fatal("replacement reused identity")
	}
	var responses []wire.Message
	status := smb.StatusUserSessionDeleted
	if denied {
		status = smb.StatusAccessDenied
	}
	for _, message := range messages {
		result := reply{status: status}
		if !denied {
			result = owner.execute(ctx, message, compoundState{})
		}
		response, responseErr := makeResponse(message.Header, result, 1)
		if responseErr != nil {
			t.Fatal(responseErr)
		}
		responses = append(responses, response)
	}
	response, err := owner.encodePayload(responses)
	if err != nil {
		t.Fatal(err)
	}
	checkRetainedReply(t, clientKey, response, encrypted || required, status, len(requests))
	if len(owner.replyProtection) != 0 {
		t.Fatal("final replies retained keys")
	}
	if result := owner.execute(ctx, requests[0], compoundState{}); result.status != smb.StatusUserSessionDeleted {
		t.Fatal("saved reply key restored removed identity")
	}
	if response := exchange(ctx, t, client, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("replacement identity is unusable")
	}
}

func checkRetainedReply(t *testing.T, clientKey *crypt.Protector, response []byte, encrypted bool, status smb.Status, count int) {
	t.Helper()
	if encrypted {
		plain, err := clientKey.Open(response)
		if err != nil {
			t.Fatalf("reply lost encryption after replacement: %v", err)
		}
		response = plain
	}
	members, err := wire.Split(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != count {
		t.Fatal("missing denial replies")
	}
	for _, member := range members {
		if member.Header.Status != status {
			t.Fatalf("reply status: %#x, want %#x", member.Header.Status, status)
		}
		if encrypted {
			if member.Header.Flags&wire.FlagSigned != 0 {
				t.Fatal("encrypted reply was separately signed")
			}
		} else if err := clientKey.Verify(member.Raw); err != nil {
			t.Fatalf("reply lost signature after replacement: %v", err)
		}
	}
}
