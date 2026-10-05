package smbtest_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// This storage seam is unused: login and ECHO never call storage methods.
type handshakeStorage struct{ smb.Storage }

func TestReconnectLoginAgainstInProcessServer(t *testing.T) {
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	account := auth.Account{User: "backup", Password: "password"}
	srv, err := server.New(server.Options{Storage: handshakeStorage{}, State: table, Account: account, ShareName: "backup", ServerName: "server", ServerGUID: [16]byte{1}, Now: time.Now, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := srv.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
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

// inProcessConn serves one pipe end. Closing the client end, or the server
// writing after that, ends ServeConn with EOF or a closed pipe.
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
