package auth

import (
	"bytes"
	"encoding/asn1"
	"testing"
)

func exchangeMessages(t testing.TB) [][]byte {
	t.Helper()
	acceptor := testAcceptor(t, vectorAccount)
	initiator := testInitiator(t, vectorAccount)
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	negotiate, err := initiator.Start(initial)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := acceptor.Step(negotiate.Token)
	if err != nil {
		t.Fatal(err)
	}
	authenticate, err := initiator.Step(challenge.Token)
	if err != nil {
		t.Fatal(err)
	}
	final, err := acceptor.Step(authenticate.Token)
	if err != nil {
		t.Fatal(err)
	}
	return [][]byte{initial, negotiate.Token, challenge.Token, authenticate.Token, final.Token}
}

func ntlmMessages(t testing.TB) [][]byte {
	t.Helper()
	var messages [][]byte
	for _, token := range exchangeMessages(t)[1:4] {
		wrapped, err := decodeSPNEGO(token)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, wrapped.token)
	}
	return messages
}

func TestNTLMTruncation(t *testing.T) {
	for _, message := range ntlmMessages(t) {
		for length := 0; length < len(message); length++ {
			if _, err := decodeNTLM(message[:length]); err == nil {
				t.Fatalf("type %d accepted truncated length %d", message[8], length)
			}
		}
		corrupt := bytes.Clone(message)
		corrupt[0] ^= 1
		if _, err := decodeNTLM(corrupt); err == nil {
			t.Fatal("bad signature accepted")
		}
		corrupt = bytes.Clone(message)
		littleEndian.PutUint32(corrupt[8:], 4)
		if _, err := decodeNTLM(corrupt); err == nil {
			t.Fatal("unknown message type accepted")
		}
	}
}

func TestSecurityBufferBounds(t *testing.T) {
	messages := ntlmMessages(t)
	for index, offsets := range [][]int{{16, 24}, {12, 40}, {12, 20, 28, 36, 44, 52}} {
		for _, offset := range offsets {
			for _, start := range []uint32{1, 0x10000000, 0xfffffff0, 0xffffffff} {
				message := bytes.Clone(messages[index])
				littleEndian.PutUint16(message[offset:], 32)
				littleEndian.PutUint32(message[offset+4:], start)
				if _, err := decodeNTLM(message); err == nil {
					t.Fatalf("type %d accepted bad buffer at %d, start %x", index+1, offset, start)
				}
			}
		}
	}
	message := bytes.Clone(messages[2])
	// A username cannot overlap the domain buffer or the MIC.
	for _, start := range []uint32{littleEndian.Uint32(message[32:]), 72} {
		littleEndian.PutUint32(message[40:], start)
		if _, err := decodeNTLM(message); err == nil {
			t.Fatal("overlapping username accepted")
		}
	}
	message = bytes.Clone(messages[2])
	for _, offset := range []int{12, 20, 28, 36, 44, 52} {
		clear(message[offset+2 : offset+4])
	}
	if _, err := decodeNTLM(message); err != nil {
		t.Fatalf("MaxLen must be ignored on receipt: %v", err)
	}
}

func TestShortNTLMv2Response(t *testing.T) {
	message := ntlmMessages(t)[2]
	for length := 0; length < 52; length++ {
		corrupt := bytes.Clone(message)
		littleEndian.PutUint16(corrupt[20:], uint16(length))
		if _, err := decodeNTLM(corrupt); err == nil {
			t.Fatalf("accepted short response length %d", length)
		}
	}
	parsed, err := decodeNTLM(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, 1, len(parsed.blob) - 1} {
		corrupt := bytes.Clone(message)
		start := int(littleEndian.Uint32(corrupt[24:])) + 16
		corrupt[start+offset] ^= 2
		if _, err := decodeNTLM(corrupt); err == nil {
			t.Fatalf("accepted invalid response type or suffix byte %d", offset)
		}
	}
	for _, offset := range []int{12, 52} {
		corrupt := bytes.Clone(message)
		littleEndian.PutUint16(corrupt[offset:], 1)
		if _, err := decodeNTLM(corrupt); err == nil {
			t.Fatal("accepted wrong LM or session key length")
		}
	}
}

func testAppendAV(t testing.TB, data []byte, id uint16, value []byte) []byte {
	t.Helper()
	result, err := appendAV(data, id, value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAVPairs(t *testing.T) {
	valid := testAppendAV(t, nil, avComputer, encodeUTF16("Server"))
	valid = testAppendAV(t, valid, avEnd, nil)
	if _, err := decodeAV(valid); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		nil,
		{0, 0, 0},
		{0, 0, 1, 0, 0},
		{0, 0, 0, 0, 0},
		{1, 0, 255, 255, 0, 0, 0, 0},
		testAppendAV(t, testAppendAV(t, nil, avFlags, make([]byte, 4)), avFlags, make([]byte, 4)),
		testAppendAV(t, testAppendAV(t, nil, avTimestamp, make([]byte, 8)), avTimestamp, make([]byte, 8)),
	} {
		if _, err := decodeAV(data); err == nil {
			t.Fatalf("accepted malformed AV pairs %x", data)
		}
	}
	for _, test := range []struct {
		id   uint16
		size int
	}{{id: avFlags, size: 4}, {id: avTimestamp, size: 8}, {id: 8, size: 48}, {id: avBindings, size: 16}} {
		for _, size := range []int{0, test.size - 1, test.size + 1} {
			data := testAppendAV(t, testAppendAV(t, nil, test.id, make([]byte, size)), avEnd, nil)
			if _, err := decodeAV(data); err == nil {
				t.Fatalf("accepted AV %d with length %d", test.id, size)
			}
		}
	}
	duplicate := testAppendAV(t, testAppendAV(t, testAppendAV(t, nil, avFlags, make([]byte, 4)), avFlags, make([]byte, 4)), avEnd, nil)
	if _, err := decodeAV(duplicate); err == nil {
		t.Fatal("duplicate AV accepted")
	}
	unknown := testAppendAV(t, testAppendAV(t, nil, 0x8000, []byte{1}), avEnd, nil)
	if _, err := decodeAV(unknown); err != nil {
		t.Fatalf("unknown AV must be ignored: %v", err)
	}
}

func TestUTF16(t *testing.T) {
	for _, value := range []string{"", "Server", "üser😀"} {
		actual, err := decodeUTF16(encodeUTF16(value))
		if err != nil || actual != value {
			t.Fatalf("UTF-16 round trip: %q, %v", actual, err)
		}
	}
	for _, data := range [][]byte{{0}, {0, 0xd8}, {0, 0xdc}, {0, 0xd8, 65, 0}, {0, 0xdc, 0, 0xd8}} {
		if _, err := decodeUTF16(data); err == nil {
			t.Fatalf("accepted malformed UTF-16 %x", data)
		}
		av := testAppendAV(t, testAppendAV(t, nil, avComputer, data), avEnd, nil)
		if _, err := decodeAV(av); err == nil {
			t.Fatal("accepted malformed UTF-16 in target info")
		}
	}
	for _, test := range []struct {
		message int
		offset  int
	}{{message: 1, offset: 12}, {message: 2, offset: 28}, {message: 2, offset: 36}, {message: 2, offset: 44}} {
		message := bytes.Clone(ntlmMessages(t)[test.message])
		if littleEndian.Uint16(message[test.offset:]) == 0 {
			// Give the empty workstation a non-overlapping one-byte payload.
			message = append(message, 0)
			if _, err := putField(message, test.offset, len(message)-1, []byte{0}); err != nil {
				t.Fatal(err)
			}
		}
		littleEndian.PutUint16(message[test.offset:], 1)
		if _, err := decodeNTLM(message); err == nil {
			t.Fatal("NTLM accepted malformed UTF-16")
		}
	}
}

func TestSPNEGOTruncationAndSyntax(t *testing.T) {
	for _, token := range exchangeMessages(t) {
		for length := 0; length < len(token); length++ {
			if _, err := decodeSPNEGO(token[:length]); err == nil {
				t.Fatalf("accepted truncated SPNEGO length %d", length)
			}
		}
		if _, err := decodeSPNEGO(append(bytes.Clone(token), 0)); err == nil {
			t.Fatal("accepted trailing SPNEGO bytes")
		}
	}
	for _, token := range [][]byte{
		{0x60, 0},
		{0x60, 0x80, 0, 0},
		{0xa0, 2, 0x30, 0},
		{0xa1, 0},
		hexBytes(t, "a1073005a0030a0104"), // Invalid state.
		hexBytes(t, "a10c300aa0030a0100a0030a0100"),         // Duplicate state.
		hexBytes(t, "a1093007a0030a01000000"),               // Trailing inner bytes.
		hexBytes(t, "601006062b0601050502a0063004a000a200"), // Missing mechanism list.
	} {
		if _, err := decodeSPNEGO(token); err == nil {
			t.Fatalf("accepted malformed SPNEGO %x", token)
		}
	}
	if _, err := decodeSPNEGO(make([]byte, maxTokenSize+1)); err == nil {
		t.Fatal("accepted oversized SPNEGO")
	}
	if _, err := decodeNTLM(make([]byte, maxTokenSize+1)); err == nil {
		t.Fatal("accepted oversized NTLM")
	}
}

func TestSPNEGOOnlyNTLM(t *testing.T) {
	acceptor := testAcceptor(t, vectorAccount)
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, initial, hexBytes(t, "601c06062b0601050502a0123010a00e300c060a2b06010401823702020a"))
	wrapped, err := decodeSPNEGO(initial)
	if err != nil || !wrapped.initial || len(wrapped.mechs) != 1 || !wrapped.mechs[0].Equal(ntlmOID) {
		t.Fatalf("advertisement does not contain exactly NTLM: %v", err)
	}
	kerberos := asn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}
	for _, mechanisms := range [][]asn1.ObjectIdentifier{{kerberos}} {
		token := initialWithMechanisms(t, mechanisms, negotiateMessage())
		result, stepErr := testAcceptor(t, vectorAccount).Step(token)
		requireFailure(t, result, stepErr)
	}
	mixed := initialWithMechanisms(t, []asn1.ObjectIdentifier{ntlmOID, kerberos}, negotiateMessage())
	if _, stepErr := testAcceptor(t, vectorAccount).Step(mixed); stepErr != nil {
		t.Fatalf("NTLM optimistic token was refused: %v", stepErr)
	}
	raw := negotiateMessage()
	result, err := testAcceptor(t, vectorAccount).Step(raw)
	requireFailure(t, result, err)
	wrongOID := bytes.Clone(initial)
	wrongOID[9] ^= 1
	if _, err := decodeSPNEGO(wrongOID); err == nil {
		t.Fatal("accepted the wrong GSS mechanism OID")
	}
}

func TestSPNEGOHints(t *testing.T) {
	// MS-SPNG's server-first NegTokenInit2 hint from the old server's fixture.
	token := hexBytes(t, "604806062b0601050502a03e303ca00e300c060a2b06010401823702020aa32a3028a0261b246e6f745f646566696e65645f696e5f5246433431373840706c656173655f69676e6f7265")
	wrapped, err := decodeSPNEGO(token)
	if err != nil || !wrapped.initial || len(wrapped.mechs) != 1 {
		t.Fatalf("valid negotiation hints were refused: %v", err)
	}
	if _, startErr := testInitiator(t, vectorAccount).Start(token); startErr != nil {
		t.Fatal(startErr)
	}
	corrupt := bytes.Clone(token)
	corrupt[34] = 0xa2 // HintName must have tag zero, not two.
	if _, err := decodeSPNEGO(corrupt); err == nil {
		t.Fatal("bad negotiation hint accepted")
	}
}

func initialWithMechanisms(t testing.TB, mechs []asn1.ObjectIdentifier, token []byte) []byte {
	t.Helper()
	sequence, err := asn1.Marshal(struct {
		Mechs []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
		Token []byte                  `asn1:"optional,explicit,tag:2"`
	}{Mechs: mechs, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	choice, err := wrapDER(asn1.ClassContextSpecific, 0, sequence)
	if err != nil {
		t.Fatal(err)
	}
	oid, err := asn1.Marshal(spnegoOID)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := wrapDER(asn1.ClassApplication, 0, append(oid, choice...))
	if err != nil {
		t.Fatal(err)
	}
	return initial
}

func FuzzNTLM(f *testing.F) {
	for _, message := range ntlmMessages(f) {
		f.Add(message)
	}
	f.Add([]byte("NTLMSSP\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		message, err := decodeNTLM(data)
		if err != nil {
			return
		}
		if message.kind < 1 || message.kind > 3 || len(message.raw) < 32 {
			t.Fatal("decoder accepted an invalid NTLM header")
		}
	})
}

func FuzzSPNEGO(f *testing.F) {
	for _, token := range exchangeMessages(f) {
		f.Add(token)
	}
	f.Add([]byte{0x60, 0})
	f.Add([]byte{0x60, 0x80, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		wrapped, err := decodeSPNEGO(data)
		if err != nil {
			return
		}
		if wrapped.initial && len(wrapped.mechs) == 0 {
			t.Fatal("initial token has no mechanisms")
		}
	})
}

func FuzzAVPairs(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add(testAppendAV(f, testAppendAV(f, nil, avTimestamp, make([]byte, 8)), avEnd, nil))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		pairs, err := decodeAV(data)
		if err != nil {
			return
		}
		for id, value := range pairs {
			if err := validateAV(id, value); err != nil {
				t.Fatal("decoder accepted an invalid AV pair")
			}
		}
	})
}

func FuzzUTF16(f *testing.F) {
	f.Add(encodeUTF16("üser😀"))
	f.Add([]byte{0, 0xd8})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := decodeUTF16(data)
		if err != nil {
			return
		}
		requireBytes(t, encodeUTF16(value), data)
	})
}
