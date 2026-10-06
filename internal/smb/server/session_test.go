package server

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

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
	response := client.negotiate(t)
	// Leasing and large MTU, nothing else.
	if response.Dialect != smb.Dialect311 || response.SecurityMode != smb.AdvertisedSecurityMode || response.ServerGUID != srv.server.options.ServerGUID || response.Capabilities != 0x06 ||
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
			client.negotiate(t)
			client.send(t, wire.Negotiate, negotiateRequest(t), 1)
		},
		"SMB1 after SMB2": func(t *testing.T, client *testClient) {
			client.negotiate(t)
			sendRaw(t, client, smb1Negotiate("\x02SMB 2.???\x00"))
		},
		"SMB1 without SMB2 offer": func(t *testing.T, client *testClient) { sendRaw(t, client, smb1Negotiate("\x02SMB 2.002\x00")) },
		"frame over 64 KiB before NEGOTIATE": func(t *testing.T, client *testClient) {
			// Only the length goes out: the server must not wait for the body.
			sendRaw(t, client, binary.BigEndian.AppendUint32(nil, 64<<10+1))
		},
		"message ID used twice": func(t *testing.T, client *testClient) {
			client.negotiate(t)
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
	client.negotiate(t)
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

// Another client is refused while the Mac is connected and while a durable
// open of the Mac waits for it to come back. More connections of the Mac
// itself are let in, so it can reconnect before its old connection is dead.
func TestOneClientAtATime(t *testing.T) {
	srv := newTestServer(t)
	mac, again := srv.connect(t), srv.connect(t)
	again.echo(t)
	other := smbtest.LoginOptions{Share: "backup", Account: srv.server.options.Account, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}}
	refused := func(when string) {
		t.Helper()
		client := srv.accept(t)
		_, err := client.raw.Login(t.Context(), other)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%#x", smb.StatusRequestNotAccepted)) {
			t.Fatalf("other client logged in %s: %v", when, err)
		}
		if err = client.ended(); err != nil {
			t.Fatal(err)
		}
	}
	refused("while the Mac is connected")
	mustCreate(t, mac, durableCreate("file", 1))
	mac.drop(t)
	again.drop(t)
	refused("while a durable open waits")
	srv.clock.advance(smb.DefaultDurableTimeout)
	srv.expire(t)
	srv.dial(t, other).echo(t)
}

// Expiry takes the opens out of the table before their storage cleanup ends.
// Until it ends, also for a delete on close, the old client still counts,
// and a cleanup that failed keeps it until the server restarts.
func TestOneClientWaitsForExpiryCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup fails %v", fail), func(t *testing.T) {
			srv := newTestServer(t)
			mac := srv.connect(t)
			mustCreate(t, mac, durableCreate("kept", 1))
			gone := durableCreate("gone", 2)
			gone.Request.Options |= fileDeleteOnClose
			mustCreate(t, mac, gone)
			mac.drop(t)
			entered, release := make(chan struct{}, 2), make(chan struct{})
			srv.faults.set(func(hooks *storageHooks) {
				hooks.Remove = func(ctx context.Context, name smb.Name, expect smb.Inode) error {
					entered <- struct{}{}
					<-release
					if fail {
						return smb.ErrIO
					}
					return srv.storage.Remove(ctx, name, expect)
				}
			})
			srv.clock.advance(smb.DefaultDurableTimeout)
			srv.server.expire(t.Context())
			<-entered
			other := smbtest.LoginOptions{Share: "backup", Account: srv.server.options.Account, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}}
			login := func() error {
				client := srv.accept(t)
				_, err := client.raw.Login(t.Context(), other)
				if err != nil {
					return errors.Join(err, client.ended())
				}
				return nil
			}
			err := login()
			close(release)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%#x", smb.StatusRequestNotAccepted)) {
				t.Fatalf("another client logged in while the old delete on close ran: %v", err)
			}
			srv.server.scavengerCleanup.Wait()
			err = login()
			switch {
			case fail && (err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%#x", smb.StatusRequestNotAccepted))):
				t.Fatalf("another client logged in after the old cleanup failed: %v", err)
			case !fail && err != nil:
				t.Fatalf("another client was refused after the old cleanup ended: %v", err)
			case !fail:
				// The old client's delete is done and none of its opens is left.
				client := srv.dial(t, other)
				if _, status := rawCreate(t, client, wire.CreateRequest{Name: "kept", DesiredAccess: fileAllAccess, Disposition: fileOpen}); status != smb.StatusSuccess {
					t.Fatalf("an open denying all sharing failed after the old cleanup: %#x", status)
				}
				if _, status := rawCreate(t, client, wire.CreateRequest{Name: "gone", DesiredAccess: 0x80, ShareAccess: 7, Disposition: fileOpen}); status != smb.StatusObjectNameNotFound {
					t.Fatalf("the old client's delete on close left the file: %#x", status)
				}
			}
		})
	}
}

// A connection keeps at most 64 logins going at a time.
func TestIncompleteLoginsAreBounded(t *testing.T) {
	client := newTestServer(t).accept(t)
	client.negotiate(t)
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
		response := client.call(t, wire.TreeConnect, encode(t, wire.EncodeTreeConnectRequest, wire.TreeConnectRequest{Path: test.path}), 1)
		header := response.Header
		if header.Status != test.want || test.want == smb.StatusSuccess && (header.TreeID == 0 || header.TreeID == client.session.TreeID) {
			t.Errorf("%s: status %#x, tree %d", test.path, header.Status, header.TreeID)
		}
		// A disk share without DFS, continuous availability or other share capabilities.
		if tree, err := wire.DecodeTreeConnectResponse(response); test.want == smb.StatusSuccess && (err != nil || tree.ShareType != 1 || tree.Flags != 0 || tree.Capabilities != 0) {
			t.Errorf("%s: tree connect reply %+v, %v", test.path, tree, err)
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
	plain.negotiate(t)
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

// An old client's requests that still run when its link drops keep it
// present: another client gets in only after they ended. So no old WRITE,
// truncate or rename can change data, sizes or names once the new client is
// in.
func TestOneClientWaitsForOldRequests(t *testing.T) {
	setInfoBody := func(class wire.FileInfoClass, id wire.FileID, input []byte) []byte {
		return encode(t, wire.EncodeSetInfoRequest, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input})
	}
	for _, test := range []struct {
		name string
		send func(t *testing.T, client *testClient, id wire.FileID)
		want map[string]string
	}{
		{"WRITE", func(t *testing.T, client *testClient, id wire.FileID) {
			client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: []byte("new")}), 1)
		}, map[string]string{"file": "new data"}},
		{"truncate", func(t *testing.T, client *testClient, id wire.FileID) {
			client.send(t, wire.SetInfo, setInfoBody(wire.ClassFileEndOfFile, id, encode(t, wire.EncodeFileEndOfFileInformation, wire.FileEndOfFileInformation{EndOfFile: 3})), 1)
		}, map[string]string{"file": "old"}},
		{"rename", func(t *testing.T, client *testClient, id wire.FileID) {
			client.send(t, wire.SetInfo, setInfoBody(wire.ClassFileRename, id, encode(t, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: "moved"})), 1)
		}, map[string]string{"file": "", "moved": "old data"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			mac := srv.connect(t)
			id := mac.open(t, "file")
			writeFile(t, mac, id, []byte("old data"))
			entered, release := make(chan struct{}), make(chan struct{})
			hold := func() error {
				close(entered)
				<-release
				return nil
			}
			srv.faults.set(func(hooks *storageHooks) {
				hooks.WriteAt = func(ctx context.Context, h smb.Handle, src []byte, offset uint64) (int, error) {
					if err := hold(); err != nil {
						return 0, err
					}
					return srv.storage.WriteAt(ctx, h, src, offset)
				}
				hooks.SetAttr = func(ctx context.Context, object smb.Inode, change smb.AttrChange) error {
					return errors.Join(hold(), srv.storage.SetAttr(ctx, object, change))
				}
				hooks.Rename = func(ctx context.Context, request smb.RenameRequest) error {
					return errors.Join(hold(), srv.storage.Rename(ctx, request))
				}
			})
			test.send(t, mac, id)
			<-entered
			// The link drops while the request runs.
			if err := mac.conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			// Also once the server has seen the link drop.
			other := smbtest.LoginOptions{Share: "backup", Account: srv.server.options.Account, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}}
			for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				client := srv.accept(t)
				_, err := client.raw.Login(t.Context(), other)
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%#x", smb.StatusRequestNotAccepted)) {
					close(release)
					t.Fatalf("another client logged in while an old %s ran: %v", test.name, err)
				}
				if err = client.ended(); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-mac.served; err != nil {
				t.Fatal(err)
			}
			mac.served = nil
			srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
			srv.dial(t, other).echo(t)
			// The drop cancels the request, so it may have ended either way,
			// but before the new client got in.
			holds := func(want map[string]string) bool {
				for name, data := range want {
					if data == "" && srv.exists(t, name) || data != "" && (!srv.exists(t, name) || srv.content(t, name) != data) {
						return false
					}
				}
				return true
			}
			before := map[string]string{"file": "old data", "moved": ""}
			if !holds(before) && !holds(test.want) {
				t.Fatalf("after the old %s, the files are neither as before nor as after it", test.name)
			}
		})
	}
}
