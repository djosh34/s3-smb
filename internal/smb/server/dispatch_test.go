package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestMissingSessionCommandsReturnStatusAndKeepConnection(t *testing.T) {
	commands := []struct {
		err     error
		body    []byte
		command wire.Command
	}{}
	add := func(command wire.Command, body []byte, err error) {
		commands = append(commands, struct {
			err     error
			body    []byte
			command wire.Command
		}{command: command, body: body, err: err})
	}
	body, err := wire.EncodeLogoffRequest(wire.EmptyRequest{})
	add(wire.Logoff, body, err)
	body, err = wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\server\\backup"})
	add(wire.TreeConnect, body, err)
	body, err = wire.EncodeTreeDisconnectRequest(wire.EmptyRequest{})
	add(wire.TreeDisconnect, body, err)
	body, err = wire.EncodeCreateRequest(wire.CreateRequest{Name: "file"})
	add(wire.Create, body, err)
	body, err = wire.EncodeCloseRequest(wire.CloseRequest{})
	add(wire.Close, body, err)
	body, err = wire.EncodeFlushRequest(wire.FlushRequest{})
	add(wire.Flush, body, err)
	body, err = wire.EncodeReadRequest(wire.ReadRequest{Length: 1})
	add(wire.Read, body, err)
	body, err = wire.EncodeWriteRequest(wire.WriteRequest{Data: []byte("x")})
	add(wire.Write, body, err)
	body, err = wire.EncodeLockRequest(wire.LockRequest{Elements: []wire.LockElement{{Length: 1, Flags: 0x12}}})
	add(wire.Lock, body, err)
	body, err = wire.EncodeIOCTLRequest(wire.IOCTLRequest{})
	add(wire.IOCTL, body, err)
	body, err = wire.EncodeQueryDirectoryRequest(wire.QueryDirectoryRequest{Pattern: "*"})
	add(wire.QueryDirectory, body, err)
	body, err = wire.EncodeChangeNotifyRequest(wire.ChangeNotifyRequest{})
	add(wire.ChangeNotify, body, err)
	body, err = wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{})
	add(wire.QueryInfo, body, err)
	body, err = wire.EncodeSetInfoRequest(wire.SetInfoRequest{})
	add(wire.SetInfo, body, err)
	body, err = wire.EncodeLeaseBreakRequest(wire.LeaseBreakRequest{})
	add(wire.OplockBreak, body, err)
	for _, command := range commands {
		if command.err != nil {
			t.Fatal(command.err)
		}
		for _, sessionID := range []uint64{0, 99} {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 1))
			message := wire.Message{Header: wire.Header{Command: command.command, MessageID: 1, SessionID: sessionID, TreeID: 42, CreditCharge: 1}, Body: command.body}
			messages := exchange(ctx, t, client, message)
			if messages[0].Header.Status != smb.StatusUserSessionDeleted {
				t.Fatalf("command %d session %d: %+v", command.command, sessionID, messages[0].Header)
			}
			messages = exchange(ctx, t, client, echo(t, 2))
			if messages[0].Header.Status != smb.StatusSuccess {
				t.Fatal("missing session dropped connection")
			}
		}
	}
}

func TestEmptyLoginReturnsFailureAndKeepsConnection(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: 1, CreditCharge: 1}, Body: body}
	messages := exchange(ctx, t, client, message)
	if messages[0].Header.Status != smb.StatusLogonFailure {
		t.Fatalf("status: %+v", messages[0].Header)
	}
	messages = exchange(ctx, t, client, echo(t, 2))
	if messages[0].Header.Status != smb.StatusSuccess {
		t.Fatal("failed login dropped connection")
	}
}

func TestCancelWithoutPendingRequestHasNoReply(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(ctx, []wire.Message{{Header: wire.Header{Command: wire.Cancel, MessageID: 1000}, Body: body}}); err != nil {
		t.Fatal(err)
	}
	messages := exchange(ctx, t, client, echo(t, 1))
	if len(messages) != 1 || messages[0].Header.Command != wire.Echo {
		t.Fatalf("CANCEL replied or consumed credits: %+v", messages)
	}
}
