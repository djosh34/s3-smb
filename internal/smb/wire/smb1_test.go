package wire

import "testing"

func TestSMB1NegotiateRequiresWildcardDialect(t *testing.T) {
	for _, test := range []struct {
		dialects string
		accepted bool
	}{
		{dialects: "\x02SMB 2.002\x00"},
		{dialects: "\x02SMB 2.???\x00", accepted: true},
		{dialects: "\x02SMB 2.002\x00\x02SMB 2.???\x00", accepted: true},
	} {
		b := builder{}
		b.bytes([]byte{0xff, 'S', 'M', 'B', 0x72})
		b.zero(27)
		b.u8(0)
		b.length16(len(test.dialects))
		b.bytes([]byte(test.dialects))
		if err := DecodeSMB1Negotiate(b.data); (err == nil) != test.accepted {
			t.Fatalf("dialects %q: %v", test.dialects, err)
		}
	}
}
