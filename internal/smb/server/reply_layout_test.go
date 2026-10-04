package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Layouts come from MS-SMB2 2.2.1 (headers), 2.2.2 (ERROR), 2.2.4 and
// 2.2.4.1 (NEGOTIATE and contexts), and 2.2.29 (ECHO). Reply values follow
// 3.3.4.2, 3.3.4.4 and 3.3.5.3.1. Requests may use encoders and the login
// fixture uses the test client, but replies under test use ReceiveRaw.
const (
	layoutHeaderSize = 64
	layoutResponse   = 0x00000001
	layoutAsync      = 0x00000002
	layoutSigned     = 0x00000008
)

type layoutField struct {
	name   string
	offset int
	width  int
	want   uint64
}

func assertLayoutFields(t *testing.T, raw []byte, fields ...layoutField) {
	t.Helper()
	for _, field := range fields {
		if field.offset < 0 || field.width < 1 || field.offset > len(raw)-field.width {
			t.Fatalf("%s at offset %d needs %d bytes, reply has %d", field.name, field.offset, field.width, len(raw))
		}
		data := raw[field.offset : field.offset+field.width]
		var got uint64
		switch field.width {
		case 1:
			got = uint64(data[0])
		case 2:
			got = uint64(binary.LittleEndian.Uint16(data))
		case 4:
			got = uint64(binary.LittleEndian.Uint32(data))
		case 8:
			got = binary.LittleEndian.Uint64(data)
		default:
			t.Fatalf("unsupported field width %d", field.width)
		}
		if got != field.want {
			t.Errorf("%s at offset %d = %#x, want %#x", field.name, field.offset, got, field.want)
		}
	}
}

type layoutHeader struct {
	status, flags, processID, treeID uint32
	messageID, sessionID, asyncID    uint64
	command, charge, credits         uint16
}

func assertLayoutHeader(t *testing.T, raw []byte, want layoutHeader) {
	t.Helper()
	if len(raw) < layoutHeaderSize {
		t.Fatalf("SMB2 header has %d bytes, want at least 64", len(raw))
	}
	if !bytes.Equal(raw[:4], []byte{0xfe, 'S', 'M', 'B'}) {
		t.Errorf("protocol ID = %x, want fe534d42", raw[:4])
	}
	assertLayoutFields(t, raw,
		layoutField{"header structure size", 4, 2, 64},
		layoutField{"credit charge", 6, 2, uint64(want.charge)},
		layoutField{"status", 8, 4, uint64(want.status)},
		layoutField{"command", 12, 2, uint64(want.command)},
		layoutField{"credit response", 14, 2, uint64(want.credits)},
		layoutField{"flags", 16, 4, uint64(want.flags)},
		layoutField{"next command", 20, 4, 0},
		layoutField{"message ID", 24, 8, want.messageID},
		layoutField{"session ID", 40, 8, want.sessionID},
	)
	if want.flags&layoutAsync != 0 {
		assertLayoutFields(t, raw, layoutField{"async ID", 32, 8, want.asyncID})
	} else {
		assertLayoutFields(t, raw,
			// The reserved field was called ProcessId in older MS-SMB2 editions.
			layoutField{"reserved (process ID)", 32, 4, uint64(want.processID)},
			layoutField{"tree ID", 36, 4, uint64(want.treeID)},
		)
	}
	zeroSignature := bytes.Equal(raw[48:64], make([]byte, 16))
	if want.flags&layoutSigned != 0 {
		if zeroSignature {
			t.Error("signed reply has zero signature at offset 48")
		}
	} else if !zeroSignature {
		t.Errorf("unsigned signature at offset 48 = %x", raw[48:64])
	}
}

func layoutExchange(ctx context.Context, t *testing.T, client *smbtest.Client, request wire.Message) []byte {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	return layoutReceive(ctx, t, client)
}

func layoutReceive(ctx context.Context, t *testing.T, client *smbtest.Client) []byte {
	t.Helper()
	raw, err := client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

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

func assertLayoutError(t *testing.T, raw []byte) {
	t.Helper()
	if len(raw) != 73 {
		t.Fatalf("ERROR reply length = %d, want 73", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"ERROR structure size", 64, 2, 9},
		layoutField{"error context count", 66, 1, 0},
		layoutField{"ERROR reserved", 67, 1, 0},
		layoutField{"error byte count", 68, 4, 0},
		layoutField{"empty error data", 72, 1, 0},
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

func layoutNegotiateOptions(t *testing.T) Options {
	t.Helper()
	options := testOptions(t)
	options.Now = func() time.Time { return time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC) }
	options.ServerGUID = [16]byte{0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87, 0x98, 0xa9, 0xba, 0xcb, 0xdc, 0xed, 0xfe, 0x0f}
	return options
}

func assertLayoutNegotiateFixed(t *testing.T, raw []byte, options Options, dialect uint16) {
	t.Helper()
	if len(raw) < 128 {
		t.Fatalf("NEGOTIATE reply length = %d, want at least 128", len(raw))
	}
	assertLayoutFields(t, raw,
		layoutField{"NEGOTIATE structure size", 64, 2, 65},
		layoutField{"security mode", 66, 2, 0x0003},
		layoutField{"dialect revision", 68, 2, uint64(dialect)},
		layoutField{"capabilities", 88, 4, 0x00000004},
		layoutField{"maximum transact size", 92, 4, 0x00100000},
		layoutField{"maximum read size", 96, 4, 0x00100000},
		layoutField{"maximum write size", 100, 4, 0x00100000},
		// FILETIME ticks from 1601-01-01 to the fixed clock above.
		layoutField{"system time", 104, 8, 134355744000000000},
		layoutField{"server start time", 112, 8, 0},
	)
	if !bytes.Equal(raw[72:88], options.ServerGUID[:]) {
		t.Errorf("server GUID at offset 72 = %x, want %x", raw[72:88], options.ServerGUID)
	}
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
