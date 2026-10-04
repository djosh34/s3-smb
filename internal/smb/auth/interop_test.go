package auth

import (
	"bytes"
	"encoding/asn1"
	"testing"
)

func clientChallenge(t testing.TB, acceptor *Acceptor, initiator *Initiator, flags uint32) ntlmMessage {
	t.Helper()
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, startErr := initiator.Start(initial); startErr != nil {
		t.Fatal(startErr)
	}
	negotiate := negotiateMessage()
	littleEndian.PutUint32(negotiate[12:], flags)
	initiator.negotiate = bytes.Clone(negotiate)
	token, err := encodeInitial(negotiate)
	if err != nil {
		t.Fatal(err)
	}
	result, err := acceptor.Step(token)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := decodeSPNEGO(result.Token)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := decodeNTLM(wrapped.token)
	if err != nil {
		t.Fatal(err)
	}
	return challenge
}

// Build a client message independently of Initiator's policy: callers choose
// the clock, flags and MIC presence, as a different NTLM implementation would.
func clientAuthenticate(t testing.TB, initiator *Initiator, challenge ntlmMessage, info, timestamp []byte, flags uint32, withMIC bool) ([]byte, []byte) {
	t.Helper()
	blob := responseBlob(timestamp, bytes.Repeat([]byte{0xaa}, 8), info)
	responseKey, err := responseKey(initiator.account, initiator.account.User, initiator.account.Domain)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ntlmHMAC(responseKey, challenge.raw[24:32], blob)
	if err != nil {
		t.Fatal(err)
	}
	baseKey, err := ntlmHMAC(responseKey, proof)
	if err != nil {
		t.Fatal(err)
	}
	key := baseKey
	var encrypted []byte
	if flags&flagKeyExch != 0 {
		key = bytes.Repeat([]byte{0x55}, 16)
		encrypted, err = exchangeKey(baseKey, key)
		if err != nil {
			t.Fatal(err)
		}
	}
	message, err := initiator.makeAuthenticate(flags, append(proof, blob...), encrypted)
	if err != nil {
		t.Fatal(err)
	}
	micOffset := 64
	if flags&flagVersion != 0 {
		micOffset += 8
	}
	if withMIC {
		mic, micErr := transcriptMIC(key, initiator.negotiate, challenge.raw, message, micOffset)
		if micErr != nil {
			t.Fatal(micErr)
		}
		copy(message[micOffset:], mic)
	} else {
		message = append(bytes.Clone(message[:micOffset]), message[micOffset+16:]...)
		for _, offset := range []int{12, 20, 28, 36, 44, 52} {
			if littleEndian.Uint16(message[offset:]) != 0 {
				start := littleEndian.Uint32(message[offset+4:])
				littleEndian.PutUint32(message[offset+4:], start-16)
			}
		}
	}
	token, err := encodeResponse(-1, nil, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	return token, key
}

func clientTargetInfo(t testing.TB, challenge ntlmMessage, withMIC bool) []byte {
	t.Helper()
	// Existing challenges may contain AV_FLAGS. Replace that pair, rather than
	// inheriting a server-imposed MIC policy in these client fixtures.
	var pairs []avPair
	for data := challenge.fields[1]; len(data) > 4; {
		id, length := littleEndian.Uint16(data), int(littleEndian.Uint16(data[2:]))
		if id != avFlags {
			pairs = append(pairs, avPair{id: id, value: data[4 : 4+length]})
		}
		data = data[4+length:]
	}
	if withMIC {
		pairs = append(pairs, avPair{id: avFlags, value: []byte{2, 0, 0, 0}})
	}
	info, err := encodeAV(pairs)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestClientClock(t *testing.T) {
	for _, withMIC := range []bool{false, true} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		challenge := clientChallenge(t, acceptor, initiator, offeredFlags)
		timestamp := hexBytes(t, "00803ed5deb19d01") // Client's Unix epoch, not the server's clock.
		token, key := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, withMIC), timestamp, challenge.flags&offeredFlags, withMIC)
		result, err := acceptor.Step(token)
		if err != nil || !result.Done {
			t.Fatalf("valid client clock was refused (MIC %v): %v", withMIC, err)
		}
		requireBytes(t, result.SessionKey, key)
	}
}

func TestChallengeNaming(t *testing.T) {
	for _, domain := range []string{"", "Domain"} {
		account := Account{User: "User", Domain: domain, Password: "Password"}
		challenge := clientChallenge(t, testAcceptor(t, account), testInitiator(t, account), offeredFlags&^flagTarget)
		requireBytes(t, challenge.av[avComputer], encodeUTF16("Server"))
		if domain == "" {
			domain = "Server"
		}
		requireBytes(t, challenge.av[avDomain], encodeUTF16(domain))
		requireBytes(t, challenge.fields[0], encodeUTF16("Server"))
		if challenge.flags&flagTarget == 0 {
			t.Error("target name was sent without REQUEST_TARGET")
		}
		if _, exists := challenge.av[avFlags]; exists {
			t.Error("challenge contains the client's MIC-present flag")
		}
	}
}

func TestOptionalTranscriptMIC(t *testing.T) {
	for _, includeFlags := range []bool{false, true} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		challenge := clientChallenge(t, acceptor, initiator, offeredFlags)
		info := clientTargetInfo(t, challenge, false)
		if includeFlags {
			info = testAppendAV(t, info[:len(info)-4], avFlags, make([]byte, 4))
			info = testAppendAV(t, info, avEnd, nil)
		}
		token, key := clientAuthenticate(t, initiator, challenge, info, challenge.av[avTimestamp], challenge.flags&offeredFlags, false)
		result, err := acceptor.Step(token)
		if err != nil || !result.Done {
			t.Fatalf("client without a transcript MIC was refused: %v", err)
		}
		requireBytes(t, result.SessionKey, key)
	}
}

func TestAuthenticateFlagSubset(t *testing.T) {
	for _, removed := range []uint32{flag128, flag56, flagVersion, flagAlwaysSign, flagKeyExch | flagSign} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		challenge := clientChallenge(t, acceptor, initiator, offeredFlags)
		flags := challenge.flags & offeredFlags &^ removed
		token, key := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, true), challenge.av[avTimestamp], flags, true)
		result, err := acceptor.Step(token)
		if err != nil || !result.Done {
			t.Fatalf("valid authentication flags were refused (%x removed): %v", removed, err)
		}
		requireBytes(t, result.SessionKey, key)
	}
}

func selectedChallenge(t testing.TB, acceptor *Acceptor, initiator *Initiator, mechanisms []asn1.ObjectIdentifier, optimistic []byte, state int) (Result, []byte) {
	t.Helper()
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, startErr := initiator.Start(initial); startErr != nil {
		t.Fatal(startErr)
	}
	offer := initialWithMechanisms(t, mechanisms, optimistic)
	result, err := acceptor.Step(offer)
	if err != nil {
		t.Fatalf("NTLM mechanism selection failed: %v", err)
	}
	selection, err := decodeSPNEGO(result.Token)
	if err != nil || selection.state != state || !selection.mechanism.Equal(ntlmOID) || len(selection.token) != 0 {
		t.Fatalf("incorrect NTLM selection response: %+v, %v", selection, err)
	}
	if result.Done || len(result.SessionKey) != 0 || result.User != "" {
		t.Fatal("selection exposed authentication before NTLM ran")
	}
	negotiate, err := encodeResponse(-1, nil, initiator.negotiate, nil)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := acceptor.Step(negotiate)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := decodeSPNEGO(challenge.Token)
	if err != nil || len(wrapped.mechanism) != 0 {
		t.Fatal("supportedMech may appear only in the first server reply")
	}
	return challenge, offer
}

func TestLaterNTLMMechanism(t *testing.T) {
	for _, variant := range []string{"valid", "bad MIC", "missing MIC"} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		kerberos := asn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}
		challenge, offer := selectedChallenge(t, acceptor, initiator, []asn1.ObjectIdentifier{kerberos, ntlmOID}, []byte("ignored optimistic Kerberos token"), 3)
		authenticate, err := initiator.Step(challenge.Token)
		if err != nil {
			t.Fatal(err)
		}
		wrapped, err := decodeSPNEGO(authenticate.Token)
		if err != nil {
			t.Fatal(err)
		}
		clientOffer, err := decodeSPNEGO(offer)
		if err != nil {
			t.Fatal(err)
		}
		mic, err := mechanismMIC(authenticate.SessionKey, clientOffer.mechList, offeredFlags, true)
		if err != nil {
			t.Fatal(err)
		}
		switch variant {
		case "bad MIC":
			mic[4] ^= 1
		case "missing MIC":
			mic = nil
		}
		token, err := encodeResponse(-1, nil, wrapped.token, mic)
		if err != nil {
			t.Fatal(err)
		}
		final, err := acceptor.Step(token)
		if variant != "valid" {
			requireFailure(t, final, err)
			continue
		}
		if err != nil || !final.Done {
			t.Fatalf("selected NTLM exchange failed: %v", err)
		}
		requireBytes(t, final.SessionKey, authenticate.SessionKey)
		wrapped, err = decodeSPNEGO(final.Token)
		if err != nil {
			t.Fatal(err)
		}
		serverMIC, err := mechanismMIC(authenticate.SessionKey, clientOffer.mechList, offeredFlags, false)
		if err != nil {
			t.Fatal(err)
		}
		requireBytes(t, wrapped.mic, serverMIC)
	}
}
