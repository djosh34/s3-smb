package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Connection tests do not perform filesystem operations.
type unusedStorage struct{ smb.Storage }

func testOptions(t *testing.T) Options {
	t.Helper()
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return Options{Storage: unusedStorage{}, State: table, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		Account: auth.Account{User: "backup", Password: "password"}, ShareName: "backup", ServerName: "s3-smb", ServerGUID: [16]byte{1}}
}

func pipeClient(t *testing.T, server *Server) (*smbtest.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return client, ctx
}

func exchange(t *testing.T, ctx context.Context, client *smbtest.Client, messages ...wire.Message) []wire.Message {
	t.Helper()
	if err := client.Send(ctx, messages); err != nil {
		t.Fatal(err)
	}
	reply, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return reply.Messages
}

func echo(t *testing.T, id uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.Echo, MessageID: id, CreditCharge: 1, Credit: 1}, Body: body}
}

func TestEcho(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	messages := exchange(t, ctx, client, echo(t, 0))
	if len(messages) != 1 || messages[0].Header.Status != smb.StatusSuccess || messages[0].Header.MessageID != 0 {
		t.Fatalf("ECHO replies: %+v", messages)
	}
	if _, err := wire.DecodeEchoResponse(messages[0]); err != nil {
		t.Fatal(err)
	}
}
