package server

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFrameLimitsRejectLengthBeforeReadingBody(t *testing.T) {
	for _, stage := range []string{"fresh", "wildcard", "negotiated"} {
		t.Run(stage, func(t *testing.T) {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			length := uint32(65537)
			switch stage {
			case "wildcard":
				sendOpeningWildcard(ctx, t, client)
			case "negotiated":
				exchange(ctx, t, client, negotiateMessage(t, 1))
				length = (1 << 20) + 65536 + 1
			}
			var prefix [4]byte
			binary.BigEndian.PutUint32(prefix[:], length)
			if err := client.SendRaw(ctx, prefix[:]); err != nil {
				t.Fatal(err)
			}
			// There is deliberately no body. The server must reject the length,
			// rather than wait for more bytes or allocate the declared payload.
			receiveCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			defer cancel()
			if _, err := client.ReceiveRaw(receiveCtx); err == nil {
				t.Fatal("oversized frame was accepted")
			}
			if err := receiveCtx.Err(); err != nil {
				t.Fatalf("server waited for the oversized body: %v", err)
			}
		})
	}
}

func TestNegotiatedFrameAllowsAdvertisedWriteSize(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 16))
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{Data: make([]byte, smb.MaxWriteSize)})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.Write, MessageID: 1, CreditCharge: 16, Credit: 16}, Body: body}
	response := exchange(ctx, t, client, message)[0]
	if response.Header.Status != smb.StatusUserSessionDeleted {
		t.Fatalf("advertised write size failed framing: %+v", response.Header)
	}
	response = exchange(ctx, t, client, echo(t, 17))[0]
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal("advertised write size closed the connection")
	}
}
