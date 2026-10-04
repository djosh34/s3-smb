package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// rawSessions permits repeated exchanges on one transport. Client.Login is
// deliberately a one-shot helper, so lifecycle tests control the transcript,
// message IDs and protection explicitly here.
type rawSessions struct {
	ctx     context.Context
	client  *smbtest.Client
	server  *Server
	preauth *crypt.Preauth
	token   []byte
	nextID  uint64
}

type rawAuthentication struct {
	preauth *crypt.Preauth
	result  auth.Result
	id      uint64
	flags   uint8
}

func newRawSessions(t *testing.T, server *Server) *rawSessions {
	t.Helper()
	client, ctx := pipeClient(t, server)
	request := negotiateMessage(t, 16)
	preauth := crypt.NewPreauth()
	payload, err := wire.Join([]wire.Message{request})
	if err != nil {
		t.Fatal(err)
	}
	preauth.Update(payload)
	response := exchange(ctx, t, client, request)[0]
	preauth.Update(response.Raw)
	negotiated, err := wire.DecodeNegotiateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return &rawSessions{ctx: ctx, client: client, server: server, preauth: preauth, token: negotiated.Token, nextID: 1}
}

func (client *rawSessions) start(t *testing.T, flags uint8) rawAuthentication {
	t.Helper()
	initiator, err := auth.NewInitiator(client.server.options.Account, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initiator.Start(client.token)
	if err != nil {
		t.Fatal(err)
	}
	preauth := client.preauth.Fork()
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, Flags: flags, SecurityMode: 3})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: client.nextID, CreditCharge: 1, Credit: 16}, Body: body}
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	preauth.Update(payload)
	response := exchange(client.ctx, t, client.client, message)[0]
	client.nextID++
	if response.Header.Status != smb.StatusMoreProcessingRequired {
		t.Fatalf("challenge: %+v", response.Header)
	}
	preauth.Update(response.Raw)
	setup, err := wire.DecodeSessionSetupResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	result, err = initiator.Step(setup.Token)
	if err != nil {
		t.Fatal(err)
	}
	return rawAuthentication{preauth: preauth, result: result, id: response.Header.SessionID, flags: flags}
}

func (client *rawSessions) finish(t *testing.T, authentication rawAuthentication, previousID uint64) *crypt.Protector {
	t.Helper()
	message := client.continuation(t, authentication, previousID)
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	authentication.preauth.Update(payload)
	response := exchange(client.ctx, t, client.client, message)[0]
	client.nextID++
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("authentication: %+v", response.Header)
	}
	protector, err := crypt.NewProtector(crypt.Options{SessionKey: authentication.result.SessionKey, Preauth: authentication.preauth.Sum(), SessionID: authentication.id, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, Role: crypt.RoleClient})
	if err != nil {
		t.Fatal(err)
	}
	if err := protector.Verify(response.Raw); err != nil {
		t.Fatal(err)
	}
	return protector
}

func (client *rawSessions) continuation(t *testing.T, authentication rawAuthentication, previousID uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: authentication.result.Token, Flags: authentication.flags, SecurityMode: 3, PreviousSessionID: previousID})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: client.nextID, SessionID: authentication.id, CreditCharge: 1, Credit: 16}, Body: body}
}

func (client *rawSessions) protected(t *testing.T, protector *crypt.Protector, message wire.Message) wire.Message {
	t.Helper()
	message.Header.MessageID = client.nextID
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	payload, err = protector.Seal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(client.ctx, t, client.client, payload)
	response, err := client.client.ReceiveRaw(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	client.nextID++
	payload, err = protector.Open(response)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatal("expected one protected reply")
	}
	return members[0]
}
