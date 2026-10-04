package server

import (
	"context"
	"encoding/asn1"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLoginRejectsWrongPasswordGuestAndMissingGCM(t *testing.T) {
	for _, test := range []struct {
		name     string
		user     string
		password string
		cipher   uint16
	}{
		{name: "password", user: "backup", password: "wrong", cipher: smb.CipherAES128GCM},
		{name: "guest", user: "Guest", password: "password", cipher: smb.CipherAES128GCM},
		{name: "empty account user", password: "password", cipher: smb.CipherAES128GCM},
		{name: "missing GCM", user: "backup", password: "password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			if _, err := client.Login(ctx, smbtest.LoginOptions{Share: "backup", Account: auth.Account{User: test.user, Password: test.password}, Signing: smb.SigningCMAC, Cipher: test.cipher}); err == nil {
				t.Fatal("login was accepted")
			}
		})
	}
}

func kerberosOffer(t *testing.T) []byte {
	t.Helper()
	sequence, err := asn1.Marshal(struct {
		Mechs []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
	}{Mechs: []asn1.ObjectIdentifier{{1, 2, 840, 113554, 1, 2, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	choice, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sequence})
	if err != nil {
		t.Fatal(err)
	}
	oid, err := asn1.Marshal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2})
	if err != nil {
		t.Fatal(err)
	}
	token, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassApplication, Tag: 0, IsCompound: true, Bytes: append(oid, choice...)})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestKerberosAndSessionBindingAreRefusedWithoutClosing(t *testing.T) {
	for _, test := range []struct {
		name   string
		token  []byte
		flags  uint8
		status smb.Status
	}{
		{name: "Kerberos", token: kerberosOffer(t), status: smb.StatusLogonFailure},
		{name: "session binding", flags: 1, status: smb.StatusRequestNotAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 16))
			body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: test.token, Flags: test.flags})
			if err != nil {
				t.Fatal(err)
			}
			response := exchange(ctx, t, client, wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: 1, CreditCharge: 1, Credit: 16}, Body: body})[0]
			if response.Header.Status != test.status {
				t.Fatalf("refusal: %+v", response.Header)
			}
			if response := exchange(ctx, t, client, echo(t, 2))[0]; response.Header.Status != smb.StatusSuccess {
				t.Fatal("refusal closed the connection")
			}
		})
	}
}

func TestTreeConnectRefusesIPCAndUnknownShares(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	paths := []string{"\\\\server\\IPC$", "\\\\server\\missing", "backup", "\\\\server\\backup\\file", "\\\\\\backup", "\\\\server\\", "\\\\server\\back/up", "\\\\ser/ver\\backup"}
	for index, path := range paths {
		body, encodeErr := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: path})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		response := exchange(ctx, t, client, wire.Message{Header: wire.Header{Command: wire.TreeConnect, MessageID: session.NextMessageID + uint64(index), SessionID: session.SessionID, CreditCharge: 1}, Body: body})[0]
		want := smb.StatusBadNetworkName
		if index >= 2 {
			want = smb.StatusInvalidParameter
		}
		if response.Header.Status != want {
			t.Fatalf("%q: %+v", path, response.Header)
		}
	}
	response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+uint64(len(paths))))[0]
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal("share refusal closed the connection")
	}
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\host\\BACKUP"})
	if err != nil {
		t.Fatal(err)
	}
	response = exchange(ctx, t, client, wire.Message{Header: wire.Header{Command: wire.TreeConnect, MessageID: session.NextMessageID + uint64(len(paths)) + 1, SessionID: session.SessionID, CreditCharge: 1}, Body: body})[0]
	if response.Header.Status != smb.StatusSuccess || response.Header.TreeID == session.TreeID {
		t.Fatal("share did not get a fresh tree ID")
	}
}

func TestTreeAndSessionChecksRejectForeignIdentifiers(t *testing.T) {
	options := testOptions(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	_, _, foreign := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	if session.SessionID == foreign.SessionID || session.TreeID == foreign.TreeID {
		t.Fatal("identifiers are not unique across connections")
	}
	var received RequestContext
	server.handlers[wire.Read] = func(_ context.Context, request RequestContext, _ wire.Message) (reply, error) {
		received = request
		return reply{status: smb.StatusNotSupported}, nil
	}
	request := treeRequest(t, session, session.NextMessageID, wire.Read)
	request.Header.TreeID = foreign.TreeID
	response := exchange(ctx, t, client, request)[0]
	if response.Header.Status != smb.StatusNetworkNameDeleted || received.Session.SessionID != 0 {
		t.Fatal("foreign tree reached a handler")
	}
	request.Header.TreeID = session.TreeID
	request.Header.MessageID++
	response = exchange(ctx, t, client, request)[0]
	if response.Header.Status != smb.StatusNotSupported || received.Binding() != (state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}) || received.Session.User != options.Account.User || received.Tree.Share != options.ShareName || received.Session.ClientGUID != (state.GUID{2}) || !received.Session.Encrypted {
		t.Fatalf("handler identity: %+v", received)
	}
	// Foreign sessions cannot authenticate transforms on this connection. Send a
	// plaintext identifier to check the status path separately from protection.
	plain, plainCtx := pipeClient(t, server)
	exchange(plainCtx, t, plain, negotiateMessage(t, 1))
	request.Header.MessageID, request.Header.SessionID = 1, foreign.SessionID
	response = exchange(plainCtx, t, plain, request)[0]
	if response.Header.Status != smb.StatusUserSessionDeleted {
		t.Fatal("foreign session was accepted")
	}
}

func TestPreviousSessionIDAllowsFreshLogin(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	oldClient, _, old := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	if closeErr := oldClient.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	client, ctx := pipeClient(t, server)
	fresh, err := client.Login(ctx, smbtest.LoginOptions{Share: "backup", Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningCMAC, PreviousSessionID: old.SessionID, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionID == old.SessionID {
		t.Fatal("reconnect reused a session ID")
	}
	if response := exchange(ctx, t, client, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("fresh session cannot send")
	}
}
