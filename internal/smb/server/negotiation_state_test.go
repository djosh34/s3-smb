package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb/wire"
)

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

func TestWildcardConsumesMessageIDZero(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	sendOpeningWildcard(ctx, t, client)
	if err := client.Send(ctx, []wire.Message{negotiateMessage(t, 1)}); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Receive(ctx); err == nil {
		t.Fatalf("wildcard left MessageId 0 available: %+v", response.Messages)
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
