// SPDX-License-Identifier: AGPL-3.0-only
package smb2

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	client "github.com/hirochachacha/go-smb2"
)

// A protocol fixture, not a captured Apple packet. SMB1's initial multiprotocol
// negotiate asks to upgrade to SMB2; it does not request SMB1 filesystem access.
func multiprotocolNegotiate() []byte {
	dialects := []byte("\x02NT LM 0.12\x00\x02SMB 2.002\x00\x02SMB 2.???\x00")
	packet := make([]byte, 35+len(dialects))
	copy(packet, []byte{0xff, 'S', 'M', 'B', byte(SMB_COM_NEGOTIATE)})
	packet[9] = 0x18
	binary.LittleEndian.PutUint16(packet[10:12], 0xc853)
	// WordCount at 32 is zero; ByteCount precedes the dialect strings.
	binary.LittleEndian.PutUint16(packet[33:35], uint16(len(dialects)))
	copy(packet[35:], dialects)
	return packet
}

func TestInvalidSMB1RequestsDisconnectWire(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"short-magic":   func(p []byte) []byte { return p[:3] },
		"short-header":  func(p []byte) []byte { return p[:4] },
		"other-command": func(p []byte) []byte { p[4] = 0x73; return p },
		"response":      func(p []byte) []byte { p[9] |= 0x80; return p },
		"word-count":    func(p []byte) []byte { p[32] = 1; return p },
		"byte-count":    func(p []byte) []byte { p[33]++; return p },
	} {
		t.Run(name, func(t *testing.T) {
			_, listener := startWireServer(t, "")
			connection, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			wire := direct(connection)
			if _, err := wire.Write(mutate(multiprotocolNegotiate())); err != nil {
				t.Fatal(err)
			}
			if _, err := wire.ReadSize(); err == nil {
				t.Fatal("invalid or non-negotiate SMB1 request accepted")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("invalid SMB1 request hung connection")
			}
		})
	}
}

func TestMultiprotocolNegotiateIsInitialOnlyWire(t *testing.T) {
	for name, initial := range map[string][]byte{
		"after-SMB1-upgrade":   multiprotocolNegotiate(),
		"after-SMB2-negotiate": requestBytes(&NegotiateRequest{Dialects: []uint16{SMB210}}),
	} {
		t.Run(name, func(t *testing.T) {
			_, listener := startWireServer(t, "")
			connection, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			wire := direct(connection)
			if _, err := wire.Write(initial); err != nil {
				t.Fatal(err)
			}
			size, err := wire.ReadSize()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wire.Read(make([]byte, size)); err != nil {
				t.Fatal(err)
			}
			if _, err := wire.Write(multiprotocolNegotiate()); err != nil {
				t.Fatal(err)
			}
			if _, err := wire.ReadSize(); err == nil {
				t.Fatal("SMB1 request accepted after initial negotiation")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("non-initial SMB1 request hung connection")
			}
		})
	}
}

func TestMultiprotocolNegotiateThenNamedEmptySignedMountWire(t *testing.T) {
	_, listener := startWireServer(t, "")
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	wire := direct(connection)
	if _, err = wire.Write(multiprotocolNegotiate()); err != nil {
		t.Fatal(err)
	}
	size, err := wire.ReadSize()
	if err != nil {
		t.Fatalf("initial SMB1 multiprotocol negotiate disconnected instead of SMB2 upgrade: %v", err)
	}
	response := make([]byte, size)
	if _, err = wire.Read(response); err != nil {
		t.Fatal(err)
	}
	packet := PacketCodec(response)
	if packet.IsInvalid() || packet.IsSmb1() || packet.Command() != SMB2_NEGOTIATE || packet.Status() != 0 {
		t.Fatalf("expected successful SMB2 negotiate response, got %x", response)
	}
	if dialect := NegotiateResponseDecoder(packet.Data()).DialectRevision(); dialect != SMB2 {
		t.Fatalf("upgrade dialect = %#x, want SMB2 wildcard %#x", dialect, SMB2)
	}
	// Continue on the same TCP connection with the ordinary SMB2 client. The
	// existing signing-required, named-empty authentication route must survive.
	dialer := &client.Dialer{
		Negotiator: client.Negotiator{RequireMessageSigning: true},
		Initiator:  &client.NTLMInitiator{User: "backup", Password: ""},
	}
	session, err := dialer.Dial(connection)
	if err != nil {
		t.Fatal(err)
	}
	share, err := session.Mount("backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := share.Umount(); err != nil {
		t.Fatal(err)
	}
}
