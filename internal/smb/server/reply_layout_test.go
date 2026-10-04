package server

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestEchoReplyByteLayout(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	layoutExchange(ctx, t, client, negotiateMessage(t, 1))
	request := echo(t, 1)
	request.Header.ProcessID = 0x12345678
	request.Header.TreeID = 0x90abcdef
	request.Header.Credit = 7
	raw := layoutExchange(ctx, t, client, request)
	assertLayoutHeader(t, raw, layoutHeader{
		flags: layoutResponse, command: 0x000d, charge: 1, credits: 7,
		messageID: 1, processID: 0x12345678, treeID: 0x90abcdef,
	})
	if len(raw) != 68 {
		t.Fatalf("ECHO reply length = %d, want 68", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"ECHO structure size", 64, 2, 4},
		layoutField{"ECHO reserved", 66, 2, 0},
	)
}

func TestErrorReplyByteLayout(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	layoutExchange(ctx, t, client, negotiateMessage(t, 1))
	request := echo(t, 1)
	request.Header.SessionID = 0x1234567890abcdef
	request.Header.ProcessID = 0x76543210
	request.Header.TreeID = 0xfedcba98
	request.Header.Credit = 3
	raw := layoutExchange(ctx, t, client, request)
	assertLayoutHeader(t, raw, layoutHeader{
		status: 0xc0000203, flags: layoutResponse, command: 0x000d, charge: 1, credits: 3,
		messageID: 1, sessionID: 0x1234567890abcdef, processID: 0x76543210, treeID: 0xfedcba98,
	})
	assertLayoutError(t, raw)
}

func TestPendingReplyByteLayout(t *testing.T) {
	// Controlled work cannot finish until the interim reply has been checked.
	// This tests the connection's async header without adding a file handler.
	server, release := controlledAsync(t, wire.Flush, reply{status: smb.StatusFileLockConflict}, nil)
	// Configure signed plaintext before serving so ReceiveRaw sees SMB2 bytes.
	// Login still authenticates and connects the share through the real server.
	server.options.Encryption = AllowPlaintext
	client, ctx, session := loginClient(t, server, 0, smb.SigningGMAC)
	request := asyncMessage(t, wire.Flush, session.NextMessageID+1)
	request.Header.SessionID = session.SessionID
	request.Header.ProcessID = 0x76543210
	request.Header.TreeID = session.TreeID
	raw := layoutExchange(ctx, t, client, request)
	assertLayoutError(t, raw)
	asyncID := binary.LittleEndian.Uint64(raw[32:40])
	if asyncID == 0 {
		t.Fatal("interim reply has zero async ID at offset 32")
	}
	if asyncID == request.Header.MessageID {
		t.Fatal("layout fixture needs distinct message and async IDs")
	}
	want := layoutHeader{
		status: 0x00000103, flags: layoutResponse | layoutAsync, command: 0x0007,
		charge: 1, credits: 16, messageID: request.Header.MessageID, sessionID: session.SessionID, asyncID: asyncID,
	}
	assertLayoutHeader(t, raw, want)

	// ECHO still completes while the controlled operation remains pending.
	echoRaw := layoutExchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID))
	assertLayoutHeader(t, echoRaw, layoutHeader{
		flags: layoutResponse | layoutSigned, command: 0x000d, charge: 1, credits: 1,
		messageID: session.NextMessageID, sessionID: session.SessionID,
	})
	close(release)
	final := layoutReceive(ctx, t, client)
	want.status, want.credits = 0xc0000054, 0
	want.flags |= layoutSigned
	assertLayoutHeader(t, final, want)
	assertLayoutError(t, final)
}

func TestSMB1WildcardReplyByteLayout(t *testing.T) {
	options := layoutNegotiateOptions(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	// SMB1 NEGOTIATE: 32-byte header, zero WordCount, ByteCount and dialects.
	payload := make([]byte, 35)
	copy(payload, []byte{0xff, 'S', 'M', 'B', 0x72})
	dialects := []byte("\x02NT LM 0.12\x00\x02SMB 2.???\x00")
	binary.LittleEndian.PutUint16(payload[33:35], 23)
	payload = append(payload, dialects...)
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, 58)
	if err := client.SendRaw(ctx, append(frame, payload...)); err != nil {
		t.Fatal(err)
	}
	raw := layoutReceive(ctx, t, client)
	assertLayoutHeader(t, raw, layoutHeader{flags: layoutResponse, command: 0, credits: 1})
	assertLayoutNegotiateFixed(t, raw, options, 0x02ff)
	if len(raw) != 128 {
		t.Fatalf("wildcard reply length = %d, want 128", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"wildcard context count", 70, 2, 0},
		layoutField{"wildcard security buffer offset", 120, 2, 128},
		layoutField{"wildcard security buffer length", 122, 2, 0},
		layoutField{"wildcard context offset", 124, 4, 0},
	)
}

func TestNegotiate311ReplyByteLayout(t *testing.T) {
	options := layoutNegotiateOptions(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	raw := layoutExchange(ctx, t, client, negotiateMessage(t, 5))
	assertLayoutHeader(t, raw, layoutHeader{flags: layoutResponse, command: 0, credits: 5})
	assertLayoutNegotiateFixed(t, raw, options, 0x0311)
	if len(raw) != 236 {
		t.Fatalf("3.1.1 reply length = %d, want 236", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"negotiate context count", 70, 2, 3},
		layoutField{"security buffer offset", 120, 2, 128},
		layoutField{"security buffer length", 122, 2, 30},
		layoutField{"negotiate context offset", 124, 4, 160},
	)
	// RFC 4178 NegTokenInit with only the NTLMSSP OID, encoded as DER.
	token := []byte{
		0x60, 0x1c, 0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02,
		0xa0, 0x12, 0x30, 0x10, 0xa0, 0x0e, 0x30, 0x0c, 0x06, 0x0a,
		0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a,
	}
	if !bytes.Equal(raw[128:158], token) {
		t.Errorf("security buffer at offset 128 = %x, want %x", raw[128:158], token)
	}
	// Context headers start at 160, 208 and 224, each on an 8-byte boundary.
	// DataLength does not count the padding after the data.
	assertLayoutFields(t, raw,
		layoutField{"preauth context type", 160, 2, 0x0001},
		layoutField{"preauth data length", 162, 2, 38},
		layoutField{"preauth reserved", 164, 4, 0},
		layoutField{"preauth hash count", 168, 2, 1},
		// The random salt occupies exactly offsets 174:206.
		layoutField{"preauth salt length", 170, 2, 32},
		layoutField{"preauth SHA-512 algorithm", 172, 2, 0x0001},
		layoutField{"encryption context type", 208, 2, 0x0002},
		layoutField{"encryption data length", 210, 2, 4},
		layoutField{"encryption reserved", 212, 4, 0},
		layoutField{"cipher count", 216, 2, 1},
		layoutField{"AES-256-GCM cipher", 218, 2, 0x0004},
		layoutField{"signing context type", 224, 2, 0x0008},
		layoutField{"signing data length", 226, 2, 4},
		layoutField{"signing reserved", 228, 4, 0},
		layoutField{"signing algorithm count", 232, 2, 1},
		layoutField{"AES-GMAC algorithm", 234, 2, 0x0002},
	)
	for _, padding := range []struct{ start, end int }{{158, 160}, {206, 208}, {220, 224}} {
		if !bytes.Equal(raw[padding.start:padding.end], make([]byte, padding.end-padding.start)) {
			t.Errorf("nonzero 8-byte alignment padding at offsets %d:%d", padding.start, padding.end)
		}
	}
}
