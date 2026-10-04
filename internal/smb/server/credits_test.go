package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Regression for #134: zero CreditRequest still grants an initial credit.
func TestNegotiateZeroRequestGrantsCredit(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	messages := exchange(ctx, t, client, negotiateMessage(t, 0))
	if messages[0].Header.Credit < 1 {
		t.Fatal("NEGOTIATE granted zero credits")
	}
	request := echo(t, 1)
	request.Header.Credit = 0
	messages = exchange(ctx, t, client, request)
	if messages[0].Header.Status != smb.StatusSuccess || messages[0].Header.Credit < 1 {
		t.Fatalf("last credit ECHO: %+v", messages[0].Header)
	}
}

// Regression for #133: synchronous compounds replenish every consumed charge.
func TestSynchronousCompoundCannotDrainLastCredits(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	messages := exchange(ctx, t, client, negotiateMessage(t, 3))
	balance := int(messages[0].Header.Credit)
	for id := uint64(1); id < 151; id += 3 {
		body, err := wire.EncodeReadRequest(wire.ReadRequest{Length: 65537})
		if err != nil {
			t.Fatal(err)
		}
		read := wire.Message{Header: wire.Header{Command: wire.Read, MessageID: id, CreditCharge: 2}, Body: body}
		echo := echo(t, id+2)
		echo.Header.Credit = 0
		messages = exchange(ctx, t, client, read, echo)
		balance -= 3
		if len(messages) != 2 || messages[0].Header.Status != smb.StatusUserSessionDeleted || messages[1].Header.Status != smb.StatusSuccess {
			t.Fatalf("compound: %+v", messages)
		}
		for _, message := range messages {
			balance += int(message.Header.Credit)
		}
		if balance <= 0 {
			t.Fatalf("compound drained balance: %d", balance)
		}
	}
}

func TestCreditGrowthIsBounded(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	messages := exchange(ctx, t, client, negotiateMessage(t, 65535))
	if messages[0].Header.Credit != smb.TargetCredits {
		t.Fatalf("initial grant: %d", messages[0].Header.Credit)
	}
	balance := int(messages[0].Header.Credit)
	for id := uint64(1); id < 20; id++ {
		request := echo(t, id)
		request.Header.Credit = 65535
		messages = exchange(ctx, t, client, request)
		balance += int(messages[0].Header.Credit) - 1
		if balance < 1 || balance > int(smb.TargetCredits) {
			t.Fatalf("credit balance %d outside target", balance)
		}
	}
}
