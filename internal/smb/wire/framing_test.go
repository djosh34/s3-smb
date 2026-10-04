package wire

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestHeaderLayouts(t *testing.T) {
	headers := []Header{
		{Command: Write, MessageID: 19, SessionID: 21, TreeID: 3, ProcessID: 5, CreditCharge: 2, Credit: 7, ChannelSequence: 3, Signature: [16]byte{1, 2}},
		{Command: Read, Flags: FlagResponse | FlagAsync, MessageID: 23, SessionID: 25, AsyncID: 27, Status: smb.StatusPending, Credit: 5},
	}
	for _, h := range headers {
		b, err := EncodeHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != 64 {
			t.Fatal(len(b))
		}
		got, err := DecodeHeader(b)
		if err != nil || got != h {
			t.Fatalf("header: %+v, %v", got, err)
		}
		for n := 0; n < 64; n++ {
			if _, err := DecodeHeader(b[:n]); err == nil {
				t.Fatalf("accepted %d bytes", n)
			}
		}
	}
	invalid := []Header{{AsyncID: 1}, {Flags: FlagAsync, TreeID: 1}, {Status: smb.StatusPending}, {Flags: FlagResponse, ChannelSequence: 1}, {NextCommand: 65}}
	for _, h := range invalid {
		if _, err := EncodeHeader(h); err == nil {
			t.Fatalf("accepted %+v", h)
		}
	}
}

func TestCompoundValidationAndOwnership(t *testing.T) {
	messages := []Message{{Header: Header{Command: Echo}, Body: []byte{4, 0, 0, 0}}, {Header: Header{Command: Command(0xffff)}, Body: []byte{1, 2}}}
	packet, err := Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Split(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Header.NextCommand != 72 || !reflect.DeepEqual(got[1].Body, messages[1].Body) {
		t.Fatalf("members: %+v", got)
	}
	raw := clone(got[0].Raw)
	packet[64] = 9
	if !bytes.Equal(got[0].Raw, raw) || got[0].Body[0] != 4 {
		t.Fatal("returned data aliases packet")
	}
	for _, offset := range []uint32{1, 64, 65, 80, 0xfffffff8} {
		bad := clone(packet)
		binary.LittleEndian.PutUint32(bad[20:], offset)
		if members, err := Split(bad); err == nil || members != nil {
			t.Fatalf("accepted offset %d", offset)
		}
	}
	bad := clone(packet)
	bad[72] = 0
	if members, err := Split(bad); err == nil || members != nil {
		t.Fatal("returned a partial chain")
	}
}

func TestSMB1OpeningNegotiate(t *testing.T) {
	b := builder{}
	b.bytes([]byte{0xff, 'S', 'M', 'B', 0x72})
	b.zero(27)
	b.u8(0)
	dialects := []byte("\x02NT LM 0.12\x00\x02SMB 2.???\x00")
	b.u16(uint16(len(dialects)))
	b.bytes(dialects)
	if err := DecodeSMB1Negotiate(b.data); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(b.data); n++ {
		if err := DecodeSMB1Negotiate(b.data[:n]); err == nil {
			t.Fatalf("accepted %d bytes", n)
		}
	}
	b.data[4] = 0x73
	if err := DecodeSMB1Negotiate(b.data); err == nil {
		t.Fatal("accepted another command")
	}
}
