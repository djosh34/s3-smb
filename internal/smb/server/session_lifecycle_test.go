package server

import (
	"encoding/asn1"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSixtyFiveLoginAndLogoffCyclesReleaseSessionSlots(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newRawSessions(t, server)
	for range 65 {
		authentication := client.start(t, 0)
		protector := client.finish(t, authentication, 0)
		session := smbtest.Session{SessionID: authentication.id}
		tree := client.protected(t, protector, treeRequest(t, session, 0, wire.TreeConnect))
		if tree.Header.Status != smb.StatusSuccess {
			t.Fatal(tree.Header)
		}
		response := client.protected(t, protector, treeRequest(t, session, 0, wire.Logoff))
		if response.Header.Status != smb.StatusSuccess {
			t.Fatal(response.Header)
		}
	}
	owner := onlyConnection(t, server)
	owner.sessionMu.RLock()
	remaining := len(owner.sessions)
	owner.sessionMu.RUnlock()
	server.mu.Lock()
	globalRemaining := len(server.sessions)
	server.mu.Unlock()
	if remaining != 0 || globalRemaining != 0 {
		t.Fatalf("logged-off sessions retained: local %d global %d", remaining, globalRemaining)
	}
	if response := exchange(client.ctx, t, client.client, echo(t, client.nextID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("cycles closed the transport")
	}
}

func onlyConnection(t *testing.T, server *Server) *connection {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.connections) != 1 {
		t.Fatal("expected one connection")
	}
	for owner := range server.connections {
		return owner
	}
	t.Fatal("connection was not registered")
	return nil
}

func TestLogoffDiscardsIncompleteAuthentication(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newRawSessions(t, server)
	incomplete := client.start(t, 0)
	request := treeRequest(t, smbtest.Session{SessionID: incomplete.id}, client.nextID, wire.Logoff)
	response := exchange(client.ctx, t, client.client, request)[0]
	client.nextID++
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	owner := onlyConnection(t, server)
	owner.sessionMu.RLock()
	remaining := len(owner.sessions)
	owner.sessionMu.RUnlock()
	if remaining != 0 {
		t.Fatal("LOGOFF retained the incomplete exchange")
	}
	response = exchange(client.ctx, t, client.client, client.continuation(t, incomplete, 0))[0]
	client.nextID++
	if response.Header.Status != smb.StatusUserSessionDeleted {
		t.Fatal("logged-off authentication continued")
	}
	fresh := client.start(t, 0)
	protector := client.finish(t, fresh, 0)
	if response := client.protected(t, protector, sessionEcho(t, smbtest.Session{SessionID: fresh.id}, 0)); response.Header.Status != smb.StatusSuccess {
		t.Fatal("session slot was not usable")
	}
}

func TestSessionSetupIgnoresUndefinedFlagBits(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newRawSessions(t, server)
	authentication := client.start(t, 0x80)
	protector := client.finish(t, authentication, 0)
	if response := client.protected(t, protector, sessionEcho(t, smbtest.Session{SessionID: authentication.id}, 0)); response.Header.Status != smb.StatusSuccess {
		t.Fatal("undefined setup flag bits prevented login")
	}
}

func TestAnonymousNTLMAuthenticationIsRefusedWithoutClosing(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newRawSessions(t, server)
	authentication := client.start(t, 0)
	// MS-NLMP anonymous AUTHENTICATE: NEGOTIATE_ANONYMOUS, a one-byte
	// zero LM response, and empty user, domain and NT challenge response.
	ntlm := make([]byte, 73)
	copy(ntlm, []byte("NTLMSSP\x00"))
	binary.LittleEndian.PutUint32(ntlm[8:], 3)
	binary.LittleEndian.PutUint16(ntlm[12:], 1)
	binary.LittleEndian.PutUint16(ntlm[14:], 1)
	binary.LittleEndian.PutUint32(ntlm[16:], 72)
	binary.LittleEndian.PutUint32(ntlm[60:], 0x02088a01)
	copy(ntlm[64:72], []byte{10, 0, 0, 0, 0, 0, 0, 15})
	sequence, err := asn1.Marshal(struct {
		Token []byte `asn1:"explicit,tag:2"`
	}{Token: ntlm})
	if err != nil {
		t.Fatal(err)
	}
	token, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, IsCompound: true, Bytes: sequence})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: token, SecurityMode: 3})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.SessionSetup, SessionID: authentication.id, MessageID: client.nextID, CreditCharge: 1, Credit: 16}, Body: body}
	response := exchange(client.ctx, t, client.client, message)[0]
	client.nextID++
	if response.Header.Status != smb.StatusLogonFailure {
		t.Fatal("anonymous authentication was not refused")
	}
	if response := exchange(client.ctx, t, client.client, echo(t, client.nextID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("anonymous refusal closed the transport")
	}
}
