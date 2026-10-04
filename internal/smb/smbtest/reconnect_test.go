package smbtest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func reconnectPeer(t *testing.T, serve func(net.Conn) error) net.Conn {
	t.Helper()
	conn, peer := net.Pipe()
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- errors.Join(serve(peer), peer.Close()) }()
	t.Cleanup(func() {
		if err := errors.Join(conn.Close(), <-done, peer.Close()); err != nil {
			t.Error(err)
		}
	})
	return conn
}

func retainedOpens(guid [16]byte) []smbtest.RetainedOpen {
	return []smbtest.RetainedOpen{
		{ID: wire.FileID{Persistent: 2, Volatile: 3}, CreateGUID: [16]byte{4}, ClientGUID: guid, Lease: wire.LeaseContext{Version: 2, Key: [16]byte{5}, State: 7, Epoch: 9}, Request: wire.CreateRequest{Name: "band1", DesiredAccess: 0x12019f, ShareAccess: 7, Disposition: 1, Options: 0x40}},
		{ID: wire.FileID{Persistent: 6, Volatile: 7}, CreateGUID: [16]byte{8}, ClientGUID: guid, Lease: wire.LeaseContext{Version: 2, Key: [16]byte{9}, State: 3, Epoch: 2}, Request: wire.CreateRequest{Name: "band2", DesiredAccess: 0x120089, ShareAccess: 3, Disposition: 1, Options: 0x40}},
	}
}

func TestReconnectRetainedOpens(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
			t.Run(fmt.Sprintf("cipher_%d_signing_%d", cipher, signing), func(t *testing.T) { checkReconnectRetainedOpens(t, cipher, signing) })
		}
	}
}

func checkReconnectRetainedOpens(t *testing.T, cipher, signing uint16) {
	t.Helper()
	previous := smbtest.Session{SessionID: 42, ClientGUID: [16]byte{11}, Cipher: cipher, Signing: signing}
	account := auth.Account{User: "backup", Password: "password"}
	opens := retainedOpens(previous.ClientGUID)
	notification, wantBreak := breakMessage(t)
	conn := reconnectPeer(t, func(peer net.Conn) error {
		protector, err := scriptReconnectLogin(peer, previous, account)
		if err != nil {
			return err
		}
		index := uint64(0)
		for _, open := range opens {
			if openErr := scriptReconnectOpen(peer, protector, cipher != 0, index, open, notification); openErr != nil {
				return openErr
			}
			index++
		}
		message, raw, err := peerReceive(peer, protector, cipher != 0)
		if err != nil {
			return err
		}
		header := wire.Header{Command: wire.Create, MessageID: 6, SessionID: reconnectSessionID, TreeID: reconnectTreeID, CreditCharge: 1, Credit: 16, Signature: message.Header.Signature}
		if cipher == 0 {
			header.Flags |= wire.FlagSigned
		}
		return checkCreateBytes(raw, header, replayCreate(opens[0]))
	})
	// Reconnect must use the previous GUID and algorithms, not these new offers.
	client, session, results, err := smbtest.Reconnect(t.Context(), conn, previous, smbtest.LoginOptions{Share: "backup", Account: account, ClientGUID: [16]byte{88}, Signing: 99, Cipher: 99}, opens)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if session.SessionID != reconnectSessionID || session.TreeID != reconnectTreeID || session.ClientGUID != previous.ClientGUID || session.NextMessageID != 6 || session.Credits != 27 {
		t.Fatalf("new session = %+v", session)
	}
	if len(results) != len(opens) {
		t.Fatalf("reconnected %d opens", len(results))
	}
	for index, result := range results {
		if result.Reply.ID.Persistent != opens[index].ID.Persistent || result.Reply.ID.Volatile != opens[index].ID.Volatile+100 || result.Lease == nil || *result.Lease != opens[index].Lease {
			t.Fatalf("reconnect result %d = %+v", index, result)
		}
	}
	got, err := client.WaitLeaseBreak(t.Context())
	if err != nil || got != wantBreak {
		t.Fatalf("break during reconnect = %+v, error = %v", got, err)
	}
	header := wire.Header{MessageID: session.NextMessageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}
	if sendErr := client.SendCreate(t.Context(), header, replayCreate(opens[0])); sendErr != nil {
		t.Fatal(sendErr)
	}
}

func replayCreate(open smbtest.RetainedOpen) smbtest.CreateOptions {
	return smbtest.CreateOptions{Request: open.Request, Lease: &open.Lease, Durable: &wire.DurableRequest{CreateGUID: open.CreateGUID, Timeout: 120000}, Replay: true}
}

func scriptReconnectOpen(peer net.Conn, protector *crypt.Protector, encrypted bool, index uint64, open smbtest.RetainedOpen, notification wire.Message) error {
	message, raw, err := peerReceive(peer, protector, encrypted)
	if err != nil {
		return err
	}
	header := wire.Header{Command: wire.Create, MessageID: index + 4, SessionID: reconnectSessionID, TreeID: reconnectTreeID, CreditCharge: 1, Credit: 16, Signature: message.Header.Signature}
	if !encrypted {
		header.Flags |= wire.FlagSigned
	}
	if checkErr := checkCreateBytes(raw, header, smbtest.CreateOptions{Request: open.Request, Lease: &open.Lease, Reconnect: &wire.DurableReconnect{ID: open.ID, CreateGUID: open.CreateGUID}}); checkErr != nil {
		return checkErr
	}
	header.Flags = wire.FlagResponse
	header.Signature = [16]byte{}
	header.Credit = 5
	if index == 0 {
		payload, encodeErr := peerEncode(notification, protector, encrypted)
		if encodeErr != nil {
			return encodeErr
		}
		if writeErr := writePayload(peer, payload); writeErr != nil {
			return writeErr
		}
		header.Flags |= wire.FlagAsync
		header.TreeID = 0
		header.AsyncID = 30
		header.Status = smb.StatusPending
		header.Credit = 7
		body, bodyErr := wire.EncodeErrorResponse(wire.ErrorResponse{})
		if bodyErr != nil {
			return bodyErr
		}
		payload, err = peerEncode(wire.Message{Header: header, Body: body}, protector, encrypted)
		if err != nil {
			return err
		}
		if writeErr := writePayload(peer, payload); writeErr != nil {
			return writeErr
		}
		header.Status = smb.StatusSuccess
		header.Credit = 0
	}
	lease, err := wire.EncodeLeaseContext(open.Lease)
	if err != nil {
		return err
	}
	body, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile + 100}, OplockLevel: 0xff, Contexts: []wire.CreateContext{lease}})
	if err != nil {
		return err
	}
	payload, err := peerEncode(wire.Message{Header: header, Body: body}, protector, encrypted)
	if err != nil {
		return err
	}
	return writePayload(peer, payload)
}

func TestReconnectClosesOnFailure(t *testing.T) {
	previous := smbtest.Session{SessionID: 42, ClientGUID: [16]byte{11}, Signing: smb.SigningCMAC}
	account := auth.Account{User: "backup", Password: "password"}
	for _, failure := range []string{"handshake", "status", "identity", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			conn := reconnectPeer(t, func(peer net.Conn) error {
				return scriptReconnectFailure(peer, previous, account, failure)
			})
			client, session, results, err := smbtest.Reconnect(t.Context(), conn, previous, smbtest.LoginOptions{Share: "backup", Account: account}, retainedOpens(previous.ClientGUID)[:1])
			if err == nil || client != nil || session != (smbtest.Session{}) || results != nil {
				t.Fatalf("failed reconnect = %v, %+v, %+v, %v", client, session, results, err)
			}
			if _, err := conn.Write([]byte{1}); err == nil {
				t.Fatal("failed reconnect left connection open")
			}
		})
	}
}

func scriptReconnectFailure(peer net.Conn, previous smbtest.Session, account auth.Account, failure string) error {
	if failure == "handshake" {
		_, err := readFrame(peer)
		return err
	}
	protector, err := scriptReconnectLogin(peer, previous, account)
	if err != nil {
		return err
	}
	message, _, err := peerReceive(peer, protector, false)
	if err != nil {
		return err
	}
	header := message.Header
	header.Signature = [16]byte{}
	header.Flags = wire.FlagResponse
	body := []byte{1}
	if failure == "status" {
		header.Status = smb.StatusObjectNameNotFound
	}
	if failure == "identity" {
		header.MessageID++
	}
	payload, err := peerEncode(wire.Message{Header: header, Body: body}, protector, false)
	if err != nil {
		return err
	}
	return writePayload(peer, payload)
}

func TestReconnectRejectsIdentityBeforeLogin(t *testing.T) {
	for _, previous := range []smbtest.Session{
		{}, {SessionID: 42}, {SessionID: 42, ClientGUID: [16]byte{11}},
	} {
		conn := reconnectPeer(t, func(peer net.Conn) error {
			var data [1]byte
			_, err := peer.Read(data[:])
			if !errors.Is(err, io.EOF) {
				return errors.New("invalid reconnect wrote a request")
			}
			return nil
		})
		if _, _, _, err := smbtest.Reconnect(t.Context(), conn, previous, smbtest.LoginOptions{}, retainedOpens([16]byte{22})); err == nil {
			t.Fatal("invalid reconnect identity accepted")
		}
	}
}

// This storage seam is unused: login and ECHO never call storage methods.
type handshakeStorage struct{ smb.Storage }

func TestReconnectLoginAgainstInProcessServer(t *testing.T) {
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	account := auth.Account{User: "backup", Password: "password"}
	srv, err := server.New(server.Options{Storage: handshakeStorage{}, State: table, Account: account, ShareName: "backup", ServerName: "server", ServerGUID: [16]byte{1}, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if shutdownErr := srv.Shutdown(ctx); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	options := smbtest.LoginOptions{Share: "backup", Account: account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC}
	old, err := smbtest.NewClient(inProcessConn(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := old.Login(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if previous.ClientGUID == [16]byte{} {
		t.Fatal("Login lost generated client GUID")
	}
	if closeErr := old.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	client, session, _, err := smbtest.Reconnect(t.Context(), inProcessConn(t, srv), previous, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if session.SessionID == previous.SessionID || session.TreeID == previous.TreeID || session.ClientGUID != previous.ClientGUID {
		t.Fatalf("previous = %+v, fresh = %+v", previous, session)
	}
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := client.Send(t.Context(), []wire.Message{{Header: wire.Header{Command: wire.Echo, SessionID: session.SessionID, MessageID: session.NextMessageID, CreditCharge: 1, Credit: 1}, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	reply, err := client.Receive(t.Context())
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("fresh protected echo = %+v, error = %v", reply, err)
	}
}

func inProcessConn(t *testing.T, srv *server.Server) net.Conn {
	t.Helper()
	conn, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.ServeConn(t.Context(), peer) }()
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, io.EOF) {
			t.Error(err)
		}
	})
	return conn
}
