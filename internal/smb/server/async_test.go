package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func asyncMessage(t *testing.T, command wire.Command, id uint64) wire.Message {
	t.Helper()
	var body []byte
	var err error
	switch uint16(command) {
	case uint16(wire.Read):
		body, err = wire.EncodeReadRequest(wire.ReadRequest{Length: 16})
	case uint16(wire.Write):
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{Data: []byte("data")})
	case uint16(wire.Flush):
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{})
	default:
		t.Fatalf("not an async command: %d", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: command, MessageID: id, SessionID: 77, TreeID: 12, CreditCharge: 1, Credit: 16}, Body: body}
}

func controlledAsync(t *testing.T, command wire.Command, result reply, resultErr error) (*Server, chan struct{}) {
	t.Helper()
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	// Install controlled work at the handler boundary, before ServeConn starts.
	// No file handler or authenticated session is needed to test completion.
	server.handlers[command] = func(ctx context.Context, _ wire.Message) (reply, error) {
		select {
		case <-release:
			return result, resultErr
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	return server, release
}

// Regression for #104: the async error keeps all three request identities.
func TestAsyncLockErrorRetainsIdentity(t *testing.T) {
	for _, command := range []wire.Command{wire.Read, wire.Write} {
		t.Run(commandName(command), func(t *testing.T) {
			server, release := controlledAsync(t, command, reply{status: smb.StatusFileLockConflict}, nil)
			client, ctx := pipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 2))
			request := asyncMessage(t, command, 1)
			pending := exchange(ctx, t, client, request)[0]
			if pending.Header.Status != smb.StatusPending || pending.Header.Flags&wire.FlagAsync == 0 || pending.Header.AsyncID == 0 || pending.Header.CreditCharge != request.Header.CreditCharge {
				t.Fatalf("pending: %+v", pending.Header)
			}
			// A blocked operation must not stop independent ECHO traffic.
			messages := exchange(ctx, t, client, echo(t, 2))
			if messages[0].Header.Status != smb.StatusSuccess {
				t.Fatalf("ECHO while pending: %+v", messages[0].Header)
			}
			close(release)
			final, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			header := final.Messages[0].Header
			if header.MessageID != 1 || header.SessionID != 77 || header.AsyncID != pending.Header.AsyncID || header.Flags&wire.FlagAsync == 0 || header.Status != smb.StatusFileLockConflict || header.TreeID != 0 || header.CreditCharge != request.Header.CreditCharge {
				t.Fatalf("async final lost identity: %+v", header)
			}
		})
	}
}

// Regression for #132: the final error does not allocate a second credit grant.
func TestAsyncBackendErrorGrantsNoFinalCredits(t *testing.T) {
	for _, command := range []wire.Command{wire.Read, wire.Write, wire.Flush} {
		t.Run(commandName(command), func(t *testing.T) {
			server, release := controlledAsync(t, command, reply{}, errors.New("controlled storage error"))
			client, ctx := pipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 1))
			pending := exchange(ctx, t, client, asyncMessage(t, command, 1))[0]
			if pending.Header.Status != smb.StatusPending || pending.Header.Credit != 16 {
				t.Fatalf("pending grant: %+v", pending.Header)
			}
			close(release)
			final, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			header := final.Messages[0].Header
			if header.Status != smb.StatusInternalError || header.Credit != 0 {
				t.Fatalf("async final credit/status: %+v", header)
			}
		})
	}
}

func commandName(command wire.Command) string {
	switch uint16(command) {
	case uint16(wire.Read):
		return "read"
	case uint16(wire.Write):
		return "write"
	case uint16(wire.Flush):
		return "flush"
	default:
		return "unknown"
	}
}
