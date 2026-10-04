package server

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestNegotiateContextValidation(t *testing.T) {
	for _, test := range []struct {
		alter       func(*wire.NegotiateRequest)
		name        string
		want        smb.Status
		wantSigning uint16
	}{
		{name: "missing preauth", alter: func(r *wire.NegotiateRequest) { r.Contexts = r.Contexts[1:] }, want: smb.StatusInvalidParameter},
		{name: "duplicate preauth", alter: func(r *wire.NegotiateRequest) { r.Contexts = append(r.Contexts, r.Contexts[0]) }, want: smb.StatusInvalidParameter},
		{name: "malformed preauth", alter: func(r *wire.NegotiateRequest) { r.Contexts[0].Data = []byte{1} }, want: smb.StatusInvalidParameter},
		{name: "no SHA-512 overlap", alter: func(r *wire.NegotiateRequest) { r.Contexts[0].Data = []byte{1, 0, 0, 0, 2, 0} }, want: smb.StatusSMBNoPreauthIntegrityHashOverlap},
		{name: "missing GCM context", alter: func(r *wire.NegotiateRequest) { r.Contexts = []wire.NegotiateContext{r.Contexts[0], r.Contexts[2]} }, want: smb.StatusNotSupported},
		{name: "no signing overlap", alter: func(r *wire.NegotiateRequest) { r.Contexts[2].Data = []byte{1, 0, 0, 0} }, want: smb.StatusSuccess, wantSigning: smb.SigningCMAC},
		{name: "empty signing offer", alter: func(r *wire.NegotiateRequest) { r.Contexts[2].Data = []byte{0, 0} }, want: smb.StatusInvalidParameter},
		{name: "duplicate encryption", alter: func(r *wire.NegotiateRequest) { r.Contexts = append(r.Contexts, r.Contexts[1]) }, want: smb.StatusInvalidParameter},
		{name: "duplicate signing", alter: func(r *wire.NegotiateRequest) { r.Contexts = append(r.Contexts, r.Contexts[2]) }, want: smb.StatusInvalidParameter},
		{name: "duplicate compression", alter: func(r *wire.NegotiateRequest) {
			r.Contexts = append(r.Contexts, wire.NegotiateContext{Type: 3}, wire.NegotiateContext{Type: 3})
		}, want: smb.StatusInvalidParameter},
		{name: "duplicate RDMA", alter: func(r *wire.NegotiateRequest) {
			r.Contexts = append(r.Contexts, wire.NegotiateContext{Type: 7}, wire.NegotiateContext{Type: 7})
		}, want: smb.StatusInvalidParameter},
		{name: "duplicate NETNAME", alter: func(r *wire.NegotiateRequest) {
			r.Contexts = append(r.Contexts, wire.NegotiateContext{Type: 5}, wire.NegotiateContext{Type: 5})
		}, want: smb.StatusSuccess},
		{name: "duplicate unknown", alter: func(r *wire.NegotiateRequest) {
			r.Contexts = append(r.Contexts, wire.NegotiateContext{Type: 65535, Data: []byte{1}}, wire.NegotiateContext{Type: 65535, Data: []byte{2}})
		}, want: smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			client, ctx := pipeClient(t, server)
			message := negotiateMessage(t, 1)
			request, err := wire.DecodeNegotiateRequest(message)
			if err != nil {
				t.Fatal(err)
			}
			test.alter(&request)
			message.Body, err = wire.EncodeNegotiateRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			messages := exchange(ctx, t, client, message)
			if messages[0].Header.Status != test.want {
				t.Fatalf("context status: %+v", messages[0].Header)
			}
			if test.want == smb.StatusSuccess {
				assertSelectedContexts(t, messages[0], test.wantSigning)
			}
		})
	}
}

func TestNegotiateDoesNotChooseUnofferedAlgorithms(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	message := negotiateMessage(t, 1)
	request, err := wire.DecodeNegotiateRequest(message)
	if err != nil {
		t.Fatal(err)
	}
	request.Contexts[1], err = wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: []uint16{smb.CipherAES128GCM}})
	if err != nil {
		t.Fatal(err)
	}
	request.Contexts[2], err = wire.EncodeSigningContext(wire.SigningContext{Algorithms: []uint16{smb.SigningCMAC}})
	if err != nil {
		t.Fatal(err)
	}
	message.Body, err = wire.EncodeNegotiateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	messages := exchange(ctx, t, client, message)
	response, err := wire.DecodeNegotiateResponse(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	encryption, err := wire.DecodeEncryptionContext(response.Contexts[1])
	if err != nil {
		t.Fatal(err)
	}
	signing, err := wire.DecodeSigningContext(response.Contexts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(encryption.Ciphers) != 1 || encryption.Ciphers[0] != smb.CipherAES128GCM || len(signing.Algorithms) != 1 || signing.Algorithms[0] != smb.SigningCMAC {
		t.Fatalf("unoffered selection: %+v %+v", encryption, signing)
	}
}

func TestCreditChargeRoundsAt64KiB(t *testing.T) {
	for _, length := range []uint32{65536, 65537} {
		server, err := New(testOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		client, ctx := pipeClient(t, server)
		exchange(ctx, t, client, negotiateMessage(t, 1))
		body, err := wire.EncodeReadRequest(wire.ReadRequest{Length: length})
		if err != nil {
			t.Fatal(err)
		}
		messages := exchange(ctx, t, client, wire.Message{Header: wire.Header{Command: wire.Read, MessageID: 1, CreditCharge: 1}, Body: body})
		want := smb.StatusUserSessionDeleted
		if length == 65537 {
			want = smb.StatusInvalidParameter
		}
		if messages[0].Header.Status != want {
			t.Fatalf("length %d: %+v", length, messages[0].Header)
		}
		messages = exchange(ctx, t, client, echo(t, 2))
		if messages[0].Header.Status != smb.StatusSuccess {
			t.Fatal("bad charge dropped connection")
		}
	}
}

func TestDuplicateMessageIDClosesConnection(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	exchange(ctx, t, client, echo(t, 1))
	if err := client.Send(ctx, []wire.Message{echo(t, 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err == nil {
		t.Fatal("reused credit was accepted")
	}
}

func TestFragmentedFramePreservesHeader(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	message := echo(t, 1)
	message.Header.ProcessID = 1234
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, 68)
	frame = append(frame, payload...)
	for _, fragment := range [][]byte{frame[:1], frame[1:3], frame[3:11], frame[11:]} {
		if sendErr := client.SendRaw(ctx, fragment); sendErr != nil {
			t.Fatal(sendErr)
		}
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if response.Messages[0].Header.ProcessID != 1234 || response.Messages[0].Header.MessageID != 1 {
		t.Fatalf("fragmented identity: %+v", response.Messages[0].Header)
	}
}

func TestLocalWriteCompoundGetsOneSuccessAndCreditGrant(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Write] = func(_ context.Context, _ RequestContext, message wire.Message) (reply, error) {
		request, err := wire.DecodeWriteRequest(message)
		if err != nil {
			return reply{}, err
		}
		body, err := wire.EncodeWriteResponse(wire.WriteResponse{Count: uint32(len(request.Data) & 0xffffff)})
		return reply{body: body}, err
	}
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	first, second := asyncMessage(t, wire.Write, 1), asyncMessage(t, wire.Write, 2)
	first.Header.Credit, second.Header.Credit = 0, 0
	messages := exchange(ctx, t, client, first, second)
	if len(messages) != 2 {
		t.Fatalf("local compound was not synchronous: %+v", messages)
	}
	for _, message := range messages {
		if message.Header.Status != smb.StatusSuccess || message.Header.Flags&wire.FlagAsync != 0 || message.Header.Credit != 1 {
			t.Fatalf("synchronous compound grant: %+v", message.Header)
		}
	}
	// A later ECHO is next, with no duplicate prefix or delayed async final.
	messages = exchange(ctx, t, client, echo(t, 3))
	if len(messages) != 1 || messages[0].Header.Command != wire.Echo {
		t.Fatalf("duplicate compound response: %+v", messages)
	}
}
