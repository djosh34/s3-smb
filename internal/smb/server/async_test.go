package server

import (
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Regression for #104: the async error keeps all three request identities.
func TestAsyncLockErrorRetainsIdentity(t *testing.T) {
	for _, command := range []wire.Command{wire.Create, wire.Read, wire.Write} {
		t.Run(commandName(command), func(t *testing.T) {
			server, release := controlledAsync(t, command, reply{status: smb.StatusFileLockConflict}, nil)
			client, ctx := corePipeClient(t, server)
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
	for _, command := range []wire.Command{wire.Create, wire.Read, wire.Write, wire.Flush} {
		t.Run(commandName(command), func(t *testing.T) {
			server, release := controlledAsync(t, command, reply{}, errors.New("controlled storage error"))
			client, ctx := corePipeClient(t, server)
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
