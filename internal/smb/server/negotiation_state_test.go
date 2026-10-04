package server

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func assertNegotiateFields(t *testing.T, response wire.NegotiateResponse, options Options) {
	t.Helper()
	now, err := wire.EncodeFiletime(options.Now())
	if err != nil {
		t.Fatal(err)
	}
	if response.SecurityMode != smb.AdvertisedSecurityMode || response.ServerGUID != options.ServerGUID || response.Capabilities != smb.AdvertisedCapabilities ||
		response.MaxRead != smb.MaxReadSize || response.MaxWrite != smb.MaxWriteSize || response.MaxTransact != smb.MaxTransactSize || response.SystemTime != uint64(now) {
		t.Fatalf("server negotiation fields: %+v", response)
	}
}

func smb1Frame(t *testing.T, dialects string) []byte {
	t.Helper()
	if len(dialects) > 65535 {
		t.Fatal("SMB1 dialect fixture is too long")
		return nil
	}
	payload := make([]byte, 35)
	copy(payload, []byte{0xff, 'S', 'M', 'B', 0x72})
	binary.LittleEndian.PutUint16(payload[33:], uint16(len(dialects)&0xffff))
	payload = append(payload, dialects...)
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	return append(frame, payload...)
}

func sendOpeningWildcard(ctx context.Context, t *testing.T, client *smbtest.Client) {
	t.Helper()
	if err := client.SendRaw(ctx, smb1Frame(t, "\x02SMB 2.???\x00")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNegotiationStateDisconnectsWithoutReply(t *testing.T) {
	for _, stage := range []string{"fresh", "wildcard", "negotiated"} {
		t.Run(stage, func(t *testing.T) {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			request := echo(t, 0)
			switch stage {
			case "wildcard":
				sendOpeningWildcard(ctx, t, client)
				request.Header.MessageID = 1
			case "negotiated":
				exchange(ctx, t, client, negotiateMessage(t, 1))
				request = negotiateMessage(t, 1)
				request.Header.MessageID = 1
			}
			if err := client.Send(ctx, []wire.Message{request}); err != nil {
				t.Fatal(err)
			}
			if response, err := client.Receive(ctx); err == nil {
				t.Fatalf("illegal negotiation state replied: %+v", response.Messages)
			}
		})
	}
}

func TestSMB1WithoutWildcardOfferDisconnects(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	if err := client.SendRaw(ctx, smb1Frame(t, "\x02SMB 2.002\x00")); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Receive(ctx); err == nil {
		t.Fatalf("unsupported SMB1 offer replied: %+v", response.Messages)
	}
}
