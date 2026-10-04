package auth

import (
	"bytes"
	"encoding/asn1"
	"errors"
	"testing"
)

func TestNegotiatedFlags(t *testing.T) {
	for _, removed := range []uint32{flagKeyExch, flagVersion, flag128, flag128 | flag56, flagKeyExch | flagVersion | flag128 | flag56} {
		acceptor := testAcceptor(t, vectorAccount)
		initiator := testInitiator(t, vectorAccount)
		initial, err := acceptor.InitialToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, startErr := initiator.Start(initial); startErr != nil {
			t.Fatal(startErr)
		}
		negotiate := negotiateMessage()
		littleEndian.PutUint32(negotiate[12:], offeredFlags&^removed)
		if removed&flagVersion != 0 {
			negotiate = negotiate[:32]
		}
		initiator.negotiate = bytes.Clone(negotiate)
		token, err := encodeInitial(negotiate)
		if err != nil {
			t.Fatal(err)
		}
		challenge, err := acceptor.Step(token)
		if err != nil {
			t.Fatal(err)
		}
		authenticate, err := initiator.Step(challenge.Token)
		if err != nil {
			t.Fatal(err)
		}
		final, err := acceptor.Step(authenticate.Token)
		if err != nil {
			t.Fatalf("flag removal %x: %v", removed, err)
		}
		requireBytes(t, final.SessionKey, authenticate.SessionKey)
		if removed&flagKeyExch != 0 {
			if bytes.Equal(final.SessionKey, bytes.Repeat([]byte{0x55}, 16)) {
				t.Fatal("exchange without KEY_EXCH did not export the base key")
			}
		}
		if result, err := initiator.Step(final.Token); err != nil || !result.Done {
			t.Fatalf("final flag variant failed: %v", err)
		}
	}
	for _, flags := range []uint32{0, offeredFlags &^ flagUnicode, offeredFlags &^ flagExtended, offeredFlags | flagAnonymous} {
		negotiate := negotiateMessage()
		littleEndian.PutUint32(negotiate[12:], flags)
		token, err := encodeInitial(negotiate)
		if err != nil {
			t.Fatal(err)
		}
		result, err := testAcceptor(t, vectorAccount).Step(token)
		requireFailure(t, result, err)
	}
}

func TestMechanismMICVectors(t *testing.T) {
	// Directional NTLM signing/sealing keys use the MS-NLMP 4.2.4 exported
	// session key. These signatures cover the DER list containing NTLM only.
	mechList := hexBytes(t, "300c060a2b06010401823702020a")
	for _, test := range []struct {
		hex    string
		flags  uint32
		client bool
	}{
		{hex: "0100000022a3984fefbb9c3200000000", flags: flagKeyExch | flag128, client: true},
		{hex: "01000000489ec007bda3438d00000000", flags: flagKeyExch | flag56, client: true},
		{hex: "010000003afa859b310b000300000000", flags: flagKeyExch, client: true},
		{hex: "010000007dd6da05648a73ae00000000", flags: flagKeyExch | flag128},
		{hex: "01000000ed0635b9ef101fc900000000", flags: flagKeyExch | flag56},
		{hex: "01000000b148d65eba5b830b00000000", flags: flagKeyExch},
	} {
		mic, err := mechanismMIC(bytes.Repeat([]byte{0x55}, 16), mechList, test.flags, test.client)
		if err != nil {
			t.Fatal(err)
		}
		requireBytes(t, mic, hexBytes(t, test.hex))
	}
	mic, err := mechanismMIC(bytes.Repeat([]byte{0x55}, 16), mechList, flag128, true)
	if err != nil {
		t.Fatal(err)
	}
	// With no KEY_EXCH the checksum is not RC4-encrypted.
	checksum, err := ntlmHMAC(hexBytes(t, "4788dc861b4782f35d43fd98fe1a2d39"), make([]byte, 4), mechList)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, mic[4:12], checksum[:8])
}

func TestOptionalMechanismMIC(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		acceptor := testAcceptor(t, vectorAccount)
		_, authenticate := startExchange(t, acceptor, testInitiator(t, vectorAccount))
		wrapped, err := decodeSPNEGO(authenticate.Token)
		if err != nil {
			t.Fatal(err)
		}
		clientMIC, err := mechanismMIC(authenticate.SessionKey, acceptor.mechList, offeredFlags, true)
		if err != nil {
			t.Fatal(err)
		}
		if corrupt {
			clientMIC[4] ^= 1
		}
		token, err := encodeResponse(-1, nil, wrapped.token, clientMIC)
		if err != nil {
			t.Fatal(err)
		}
		final, err := acceptor.Step(token)
		if corrupt {
			requireFailure(t, final, err)
			continue
		}
		if err != nil || !final.Done {
			t.Fatalf("valid mechanism MIC was refused: %v", err)
		}
		wrapped, err = decodeSPNEGO(final.Token)
		if err != nil {
			t.Fatal(err)
		}
		serverMIC, err := mechanismMIC(authenticate.SessionKey, acceptor.mechList, offeredFlags, false)
		if err != nil {
			t.Fatal(err)
		}
		requireBytes(t, wrapped.mic, serverMIC)
	}
}

func TestFinalSPNEGOAcceptance(t *testing.T) {
	kerberos := asn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}
	for _, test := range []struct {
		mech  asn1.ObjectIdentifier
		token []byte
		mic   []byte
		state int
	}{
		{state: -1},
		{state: 1},
		{state: 2},
		{state: 3},
		{state: 0, mech: kerberos},
		{state: 0, token: []byte{1}},
		{state: 0, mic: make([]byte, 16)},
	} {
		initiator := testInitiator(t, vectorAccount)
		startExchange(t, testAcceptor(t, vectorAccount), initiator)
		token, err := encodeResponse(test.state, test.mech, test.token, test.mic)
		if err != nil {
			t.Fatal(err)
		}
		result, err := initiator.Step(token)
		requireFailure(t, result, err)
	}
	for _, token := range [][]byte{nil, []byte("NTLMSSP\x00"), exchangeMessages(t)[0]} {
		initiator := testInitiator(t, vectorAccount)
		startExchange(t, testAcceptor(t, vectorAccount), initiator)
		result, err := initiator.Step(token)
		requireFailure(t, result, err)
	}
}

func TestNoMICDowngrade(t *testing.T) {
	acceptor := testAcceptor(t, vectorAccount)
	_, authenticate := startExchange(t, acceptor, testInitiator(t, vectorAccount))
	wrapped, err := decodeSPNEGO(authenticate.Token)
	if err != nil {
		t.Fatal(err)
	}
	message, err := decodeNTLM(wrapped.token)
	if err != nil {
		t.Fatal(err)
	}
	// Recompute a valid proof after clearing AV_FLAGS. This must not disable
	// the MIC requirement imposed by the server's timestamp.
	clear(message.av[avFlags])
	key, err := responseKey(vectorAccount, "User", "Domain")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ntlmHMAC(key, acceptor.challenge[24:32], message.blob)
	if err != nil {
		t.Fatal(err)
	}
	copy(message.fields[1], proof)
	clear(message.raw[message.micOffset : message.micOffset+16])
	token, err := encodeResponse(-1, nil, message.raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := acceptor.Step(token)
	requireFailure(t, result, err)
}

func TestExchangeState(t *testing.T) {
	var nilAcceptor *Acceptor
	var nilInitiator *Initiator
	for _, acceptor := range []*Acceptor{nilAcceptor, {}} {
		if _, err := acceptor.InitialToken(); !errors.Is(err, errExchange) {
			t.Fatalf("zero acceptor advertisement: %v", err)
		}
		result, err := acceptor.Step(nil)
		requireFailure(t, result, err)
	}
	for _, initiator := range []*Initiator{nilInitiator, {}} {
		result, err := initiator.Start(nil)
		requireFailure(t, result, err)
		result, err = initiator.Step(nil)
		requireFailure(t, result, err)
	}
	acceptor := testAcceptor(t, vectorAccount)
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, initial, second)
	initiator := testInitiator(t, vectorAccount)
	negotiate, err := initiator.Start(initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, startErr := initiator.Start(initial); !errors.Is(startErr, errExchange) {
		t.Fatal("initiator started twice")
	}
	result, err := testInitiator(t, vectorAccount).Step(initial)
	requireFailure(t, result, err)
	// An authenticate message cannot start the acceptor's exchange.
	result, err = testAcceptor(t, vectorAccount).Step(exchangeMessages(t)[3])
	requireFailure(t, result, err)
	// Malformed input consumes the exchange even before a challenge.
	result, err = acceptor.Step(nil)
	requireFailure(t, result, err)
	result, err = acceptor.Step(negotiate.Token)
	requireFailure(t, result, err)
}

func TestOwnedTranscriptBuffers(t *testing.T) {
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
	clear(negotiate.Token)
	authenticate, err := initiator.Step(challenge.Token)
	if err != nil {
		t.Fatal(err)
	}
	clear(challenge.Token)
	final, err := acceptor.Step(authenticate.Token)
	if err != nil {
		t.Fatalf("caller mutation changed the retained transcript: %v", err)
	}
	requireBytes(t, authenticate.SessionKey, final.SessionKey)
}

func TestEncoderBounds(t *testing.T) {
	for _, test := range []struct {
		value  []byte
		offset int
		start  int
	}{
		{offset: -1},
		{offset: 4},
		{start: -1},
		{start: 33},
		{value: make([]byte, maxTokenSize+1)},
		{start: 30, value: make([]byte, 3)},
	} {
		if _, err := putField(make([]byte, 8), test.offset, test.start, test.value); err == nil {
			t.Fatal("invalid security buffer encoding accepted")
		}
	}
	if _, err := appendAV(nil, avComputer, make([]byte, maxTokenSize+1)); err == nil {
		t.Fatal("oversized AV value accepted")
	}
	if _, err := encodeAV([]avPair{{value: make([]byte, maxTokenSize), id: avComputer}}); err == nil {
		t.Fatal("oversized target info accepted")
	}
	if _, err := testInitiator(t, vectorAccount).makeAuthenticate(offeredFlags, make([]byte, maxTokenSize), nil); err == nil {
		t.Fatal("oversized authenticate message accepted")
	}
	if _, err := encodeResponse(-1, nil, make([]byte, maxTokenSize), nil); err == nil {
		t.Fatal("oversized SPNEGO response accepted")
	}
	if _, err := exchangeKey(nil, make([]byte, 16)); err == nil {
		t.Fatal("invalid RC4 key accepted")
	}
}

func TestIndependentExchanges(t *testing.T) {
	for _, user := range []string{"one", "two", "three", "four"} {
		t.Run(user, func(t *testing.T) {
			t.Parallel()
			account := Account{User: user, Password: "Password"}
			acceptor := testAcceptor(t, account)
			_, authenticate := startExchange(t, acceptor, testInitiator(t, account))
			final, err := acceptor.Step(authenticate.Token)
			if err != nil || !final.Done || final.User != user {
				t.Fatalf("separate exchange failed: %v", err)
			}
		})
	}
}
