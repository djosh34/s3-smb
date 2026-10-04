package server

import (
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// negotiate sends the Mac's NEGOTIATE and returns the decoded reply.
func negotiate(t *testing.T, client *testClient) wire.NegotiateResponse {
	t.Helper()
	response, status := decodeReply(t, client.call(t, wire.Negotiate, negotiateRequest(t), 1), wire.DecodeNegotiateResponse)
	if status != smb.StatusSuccess {
		t.Fatalf("NEGOTIATE status %#x", status)
	}
	return response
}

// smb1Negotiate is the framed SMB1 NEGOTIATE a Mac opens with.
func smb1Negotiate(dialects string) []byte {
	payload := append(make([]byte, 35), dialects...)
	copy(payload, "\xffSMB\x72")
	binary.LittleEndian.PutUint16(payload[33:], uint16(len(dialects)&0xffff))
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(payload)&0xffffff)), payload...)
}

func TestNegotiate(t *testing.T) {
	srv := newTestServer(t)
	client := srv.accept(t)
	body := encode(t, wire.EncodeNegotiateRequest, wire.NegotiateRequest{Dialects: []uint16{0x0302}, SecurityMode: smb.AdvertisedSecurityMode})
	if status := client.call(t, wire.Negotiate, body, 1).Header.Status; status != smb.StatusNotSupported {
		t.Fatalf("SMB 3.0.2 NEGOTIATE status %#x", status)
	}
	response := negotiate(t, client)
	if response.Dialect != smb.Dialect311 || response.SecurityMode != smb.AdvertisedSecurityMode || response.ServerGUID != srv.server.options.ServerGUID ||
		response.MaxRead != smb.MaxReadSize || response.MaxWrite != smb.MaxWriteSize || response.MaxTransact != smb.MaxTransactSize || len(response.Token) == 0 {
		t.Fatalf("NEGOTIATE reply %+v", response)
	}
	if len(response.Contexts) != 3 {
		t.Fatalf("contexts %+v", response.Contexts)
	}
	// Of what the Mac offers, the server picks AES-256-GCM and AES-GMAC.
	preauth, preauthErr := wire.DecodePreauthContext(response.Contexts[0])
	encryption, encryptionErr := wire.DecodeEncryptionContext(response.Contexts[1])
	signing, signingErr := wire.DecodeSigningContext(response.Contexts[2])
	if err := errors.Join(preauthErr, encryptionErr, signingErr); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preauth.Hashes, []uint16{smb.PreauthSHA512}) || len(preauth.Salt) != 32 ||
		!slices.Equal(encryption.Ciphers, []uint16{smb.CipherAES256GCM}) || !slices.Equal(signing.Algorithms, []uint16{smb.SigningGMAC}) {
		t.Fatalf("contexts %+v, %+v, %+v", preauth, encryption, signing)
	}
}

func TestSelectAlgorithms(t *testing.T) {
	context := func(context wire.NegotiateContext, err error) wire.NegotiateContext {
		if err != nil {
			t.Fatal(err)
		}
		return context
	}
	preauth := context(wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}}))
	ciphers := func(ciphers ...uint16) wire.NegotiateContext {
		return context(wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: ciphers}))
	}
	signing := func(algorithms ...uint16) wire.NegotiateContext {
		return context(wire.EncodeSigningContext(wire.SigningContext{Algorithms: algorithms}))
	}
	const ccm = 1
	for _, test := range []struct {
		name            string
		contexts        []wire.NegotiateContext
		policy          EncryptionPolicy
		cipher, signing uint16
		want            smb.Status
	}{
		{"offered algorithms only", []wire.NegotiateContext{preauth, ciphers(ccm, smb.CipherAES128GCM), signing(smb.SigningCMAC)}, RequireEncryption, smb.CipherAES128GCM, smb.SigningCMAC, smb.StatusSuccess},
		{"no signing context", []wire.NegotiateContext{preauth, ciphers(smb.CipherAES256GCM)}, RequireEncryption, smb.CipherAES256GCM, smb.SigningCMAC, smb.StatusSuccess},
		{"unknown signing algorithm", []wire.NegotiateContext{preauth, ciphers(smb.CipherAES256GCM), signing(7)}, RequireEncryption, smb.CipherAES256GCM, smb.SigningCMAC, smb.StatusSuccess},
		{"no GCM, encryption required", []wire.NegotiateContext{preauth, ciphers(ccm)}, RequireEncryption, 0, 0, smb.StatusNotSupported},
		{"no GCM, plaintext allowed", []wire.NegotiateContext{preauth, ciphers(ccm), signing(smb.SigningGMAC)}, AllowPlaintext, 0, smb.SigningGMAC, smb.StatusSuccess},
		{"no preauth", []wire.NegotiateContext{ciphers(smb.CipherAES256GCM)}, AllowPlaintext, 0, 0, smb.StatusInvalidParameter},
		{"no SHA-512", []wire.NegotiateContext{context(wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{2}}))}, AllowPlaintext, 0, 0, smb.StatusSMBNoPreauthIntegrityHashOverlap},
		{"signing twice", []wire.NegotiateContext{preauth, signing(smb.SigningGMAC), signing(smb.SigningGMAC)}, AllowPlaintext, 0, 0, smb.StatusInvalidParameter},
		{"compression twice", []wire.NegotiateContext{preauth, {Type: 3}, {Type: 3}}, AllowPlaintext, 0, 0, smb.StatusInvalidParameter},
		{"unknown context twice", []wire.NegotiateContext{preauth, {Type: 0xffff}, {Type: 0xffff}}, AllowPlaintext, 0, smb.SigningCMAC, smb.StatusSuccess},
	} {
		selected, status, reason := selectAlgorithms(test.contexts, test.policy)
		if status != test.want || status == smb.StatusSuccess && (selected.cipher != test.cipher || selected.signing != test.signing) || (status == smb.StatusSuccess) != (reason == "") {
			t.Errorf("%s: %+v, %#x, %q", test.name, selected, status, reason)
		}
	}
}

// macOS opens with an SMB1 NEGOTIATE. The reply sends it on to SMB2, and the
// SMB1 exchange stays out of the preauth hash that protects the login.
func TestSMB1NegotiateMovesToSMB2(t *testing.T) {
	srv := newTestServer(t)
	client := srv.accept(t)
	if err := client.raw.SendRaw(t.Context(), smb1Negotiate("\x02NT LM 0.12\x00\x02SMB 2.???\x00")); err != nil {
		t.Fatal(err)
	}
	reply, err := client.raw.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wildcard, err := wire.DecodeNegotiateResponse(reply.Messages[0])
	if err != nil || wildcard.Dialect != smb.DialectWildcard || reply.Messages[0].Header.Credit != 1 {
		t.Fatalf("SMB1 NEGOTIATE reply %+v, %v", reply.Messages[0].Header, err)
	}
	client.session.NextMessageID = 1 // The SMB1 exchange used message ID 0.
	request := client.send(t, wire.Negotiate, negotiateRequest(t), 1)
	response := client.receive(t, request)
	if status := response.Header.Status; status != smb.StatusSuccess {
		t.Fatalf("NEGOTIATE status %#x", status)
	}
	client.echo(t)
	payload, err := wire.Join([]wire.Message{{Header: request, Body: negotiateRequest(t)}})
	if err != nil {
		t.Fatal(err)
	}
	first := sha512.Sum512(append(make([]byte, 64), payload...))
	want := sha512.Sum512(append(first[:], response.Raw...))
	srv.server.mu.Lock()
	defer srv.server.mu.Unlock()
	for connection := range srv.server.connections {
		connection.sessionMu.Lock()
		if connection.preauth.Sum() != want {
			t.Error("preauth hash does not start at the SMB2 NEGOTIATE")
		}
		connection.sessionMu.Unlock()
	}
}

// A client that breaks the order of the opening exchange, or reuses a message
// ID, loses its connection without a reply.
func TestNegotiationOrder(t *testing.T) {
	srv := newTestServer(t)
	sendRaw := func(t *testing.T, client *testClient, framed []byte) {
		t.Helper()
		if err := client.raw.SendRaw(t.Context(), framed); err != nil {
			t.Fatal(err)
		}
	}
	echo := encode(t, wire.EncodeEchoRequest, wire.EmptyRequest{})
	for name, send := range map[string]func(t *testing.T, client *testClient){
		"request before NEGOTIATE": func(t *testing.T, client *testClient) { client.send(t, wire.Echo, echo, 1) },
		"second NEGOTIATE": func(t *testing.T, client *testClient) {
			negotiate(t, client)
			client.send(t, wire.Negotiate, negotiateRequest(t), 1)
		},
		"SMB1 after SMB2": func(t *testing.T, client *testClient) {
			negotiate(t, client)
			sendRaw(t, client, smb1Negotiate("\x02SMB 2.???\x00"))
		},
		"SMB1 without SMB2 offer": func(t *testing.T, client *testClient) { sendRaw(t, client, smb1Negotiate("\x02SMB 2.002\x00")) },
		"frame over 64 KiB before NEGOTIATE": func(t *testing.T, client *testClient) {
			// Only the length goes out: the server must not wait for the body.
			sendRaw(t, client, binary.BigEndian.AppendUint32(nil, 64<<10+1))
		},
		"message ID used twice": func(t *testing.T, client *testClient) {
			negotiate(t, client)
			client.session.NextMessageID = 0
			client.send(t, wire.Echo, echo, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := srv.accept(t)
			send(t, client)
			if reply, err := client.raw.Receive(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("reply %+v, %v", reply.Messages, err)
			}
			if err := client.ended(); err == nil {
				t.Fatal("server ended the connection without an error")
			}
		})
	}
}

func TestLogin(t *testing.T) {
	for _, test := range []struct {
		name            string
		cipher, signing uint16
		policy          EncryptionPolicy
	}{
		{"signed with AES-CMAC", 0, smb.SigningCMAC, AllowPlaintext},
		{"signed with AES-GMAC", 0, smb.SigningGMAC, AllowPlaintext},
		{"AES-128-GCM", smb.CipherAES128GCM, smb.SigningCMAC, RequireEncryption},
		{"AES-256-GCM", smb.CipherAES256GCM, smb.SigningGMAC, RequireEncryption},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.server.options.Encryption = test.policy
			client := srv.dial(t, smbtest.LoginOptions{Cipher: test.cipher, Signing: test.signing})
			if client.session.Cipher != test.cipher || client.session.Signing != test.signing {
				t.Fatalf("session %+v", client.session)
			}
			// The test client checks the signature or encryption of every reply.
			client.echo(t)
		})
	}
}

func TestLoginRefusals(t *testing.T) {
	srv := newTestServer(t)
	client := srv.accept(t)
	if _, err := client.raw.Login(t.Context(), smbtest.LoginOptions{Share: "backup", Account: auth.Account{User: "backup", Password: "wrong"}, Signing: smb.SigningGMAC}); err == nil {
		t.Fatal("wrong password logged in")
	}
	if err := client.ended(); err != nil {
		t.Fatal(err)
	}
	client = srv.accept(t)
	negotiate(t, client)
	for _, test := range []struct {
		name    string
		request wire.SessionSetupRequest
		want    smb.Status
	}{
		{"no token", wire.SessionSetupRequest{}, smb.StatusLogonFailure},
		{"session binding", wire.SessionSetupRequest{Flags: 1}, smb.StatusRequestNotAccepted},
	} {
		if status := client.call(t, wire.SessionSetup, encode(t, wire.EncodeSessionSetupRequest, test.request), 1).Header.Status; status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	client.echo(t)
}

// A connection keeps at most 64 logins going at a time.
func TestIncompleteLoginsAreBounded(t *testing.T) {
	client := newTestServer(t).accept(t)
	negotiate(t, client)
	start := loginStart(t)
	for range 64 {
		if status := client.call(t, wire.SessionSetup, start, 1).Header.Status; status != smb.StatusMoreProcessingRequired {
			t.Fatalf("SESSION_SETUP status %#x", status)
		}
	}
	if status := client.call(t, wire.SessionSetup, start, 1).Header.Status; status != smb.StatusInsufficientResources {
		t.Fatalf("65th SESSION_SETUP status %#x", status)
	}
	client.echo(t)
}

// There is one disk share, found by name in any case, and no IPC$.
func TestTreeConnect(t *testing.T) {
	client := newTestServer(t).connect(t)
	for _, test := range []struct {
		path string
		want smb.Status
	}{
		{`\\host\IPC$`, smb.StatusBadNetworkName},
		{`\\host\missing`, smb.StatusBadNetworkName},
		{`backup`, smb.StatusInvalidParameter},
		{`\\host\backup\dir`, smb.StatusInvalidParameter},
		{`\\host\BACKUP`, smb.StatusSuccess},
	} {
		header := client.call(t, wire.TreeConnect, encode(t, wire.EncodeTreeConnectRequest, wire.TreeConnectRequest{Path: test.path}), 1).Header
		if header.Status != test.want || test.want == smb.StatusSuccess && (header.TreeID == 0 || header.TreeID == client.session.TreeID) {
			t.Errorf("%s: status %#x, tree %d", test.path, header.Status, header.TreeID)
		}
	}
}

// Sessions and trees belong to their connection.
func TestForeignSessionAndTree(t *testing.T) {
	srv := newTestServer(t)
	client, other := srv.connect(t), srv.connect(t)
	flush := encode(t, wire.EncodeFlushRequest, wire.FlushRequest{})
	header := client.header(wire.Flush, 1)
	header.TreeID = other.session.TreeID
	if err := client.raw.Send(t.Context(), []wire.Message{{Header: header, Body: flush}}); err != nil {
		t.Fatal(err)
	}
	if status := client.receive(t, header).Header.Status; status != smb.StatusNetworkNameDeleted {
		t.Fatalf("FLUSH on another connection's tree: status %#x", status)
	}
	plain := srv.accept(t)
	negotiate(t, plain)
	header = plain.header(wire.Flush, 1)
	header.SessionID, header.TreeID = other.session.SessionID, other.session.TreeID
	if err := plain.raw.Send(t.Context(), []wire.Message{{Header: header, Body: flush}}); err != nil {
		t.Fatal(err)
	}
	if status := plain.receive(t, header).Header.Status; status != smb.StatusUserSessionDeleted {
		t.Fatalf("FLUSH in another connection's session: status %#x", status)
	}
	client.echo(t)
	plain.echo(t)
}
