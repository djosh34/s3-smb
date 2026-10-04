package wire

import (
	"encoding/binary"
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
		got, err := DecodeHeader(b)
		if err != nil || got != h {
			t.Fatalf("header: %+v, %v", got, err)
		}
		if _, err := DecodeHeader(b[:63]); err == nil {
			t.Fatal("accepted a short header")
		}
	}
	invalid := []Header{{AsyncID: 1}, {Flags: FlagAsync, TreeID: 1}, {Status: smb.StatusPending}, {Flags: FlagResponse, ChannelSequence: 1}, {NextCommand: 65}}
	for _, h := range invalid {
		if _, err := EncodeHeader(h); err == nil {
			t.Fatalf("accepted %+v", h)
		}
	}
}

// The upper half of a request's ChannelSequence word is reserved: Split keeps
// the received bytes for signing, and the decoded header ignores them.
func TestReservedRequestHeaderWord(t *testing.T) {
	want := Header{Command: Write, ChannelSequence: 7}
	data, err := EncodeHeader(want)
	if err != nil {
		t.Fatal(err)
	}
	data[10], data[11] = 0xff, 0xee
	members, err := Split(append(data, 1, 2, 3))
	if err != nil || len(members) != 1 || members[0].Header != want {
		t.Fatalf("members: %+v, %v", members, err)
	}
	if members[0].Raw[10] != 0xff || members[0].Raw[11] != 0xee {
		t.Fatal("Split did not keep the received reserved bytes")
	}
}

func TestCompoundSplitAndJoin(t *testing.T) {
	messages := []Message{{Header: Header{Command: Write}, Body: []byte{1, 2, 3}}, {Header: Header{Command: Command(0xffff)}, Body: []byte{4, 0, 0, 0}}}
	packet, err := Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Split(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Header.NextCommand != 72 || len(got[0].Raw) != 72 || got[1].Body[0] != 4 {
		t.Fatalf("members: %+v", got)
	}
	got[0].Body[0] = 9
	if got[0].Raw[64] != 9 || packet[64] != 1 {
		t.Fatal("Body must share the member's Raw copy, not the packet")
	}
	for _, offset := range []uint32{1, 64, 65, 80, 0xfffffff8} {
		bad := clone(packet)
		binary.LittleEndian.PutUint32(bad[20:], offset)
		if members, err := Split(bad); err == nil || members != nil {
			t.Fatalf("accepted NextCommand %d", offset)
		}
	}
	bad := clone(packet)
	bad[72] = 0
	if members, err := Split(bad); err == nil || members != nil {
		t.Fatal("returned a partial chain")
	}
}

func smb1Negotiate(dialects string) []byte {
	b := builder{}
	b.bytes([]byte{0xff, 'S', 'M', 'B', 0x72})
	b.zero(27)
	b.u8(0)
	b.length16(len(dialects))
	b.bytes([]byte(dialects))
	return b.data
}

func TestSMB1Negotiate(t *testing.T) {
	for _, test := range []struct {
		dialects string
		accepted bool
	}{
		{dialects: "\x02SMB 2.002\x00"},
		{dialects: "\x02SMB 2.???\x00", accepted: true},
		{dialects: "\x02NT LM 0.12\x00\x02SMB 2.???\x00", accepted: true},
	} {
		if err := DecodeSMB1Negotiate(smb1Negotiate(test.dialects)); (err == nil) != test.accepted {
			t.Fatalf("dialects %q: %v", test.dialects, err)
		}
	}
	packet := smb1Negotiate("\x02SMB 2.???\x00")
	if err := DecodeSMB1Negotiate(packet[:len(packet)-1]); err == nil {
		t.Fatal("accepted a truncated dialect list")
	}
	packet[4] = 0x73
	if err := DecodeSMB1Negotiate(packet); err == nil {
		t.Fatal("accepted another SMB1 command")
	}
}
