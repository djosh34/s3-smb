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

func TestMechanismMICWithDropped128(t *testing.T) {
	acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
	challenge := clientChallenge(t, acceptor, initiator, offeredFlags)
	if challenge.flags&flag128 == 0 {
		t.Fatal("challenge must offer 128-bit keys for this regression")
	}
	flags := challenge.flags & offeredFlags &^ flag128
	token, key := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, true), challenge.av[avTimestamp], flags, true)
	wrapped, err := decodeSPNEGO(token)
	if err != nil {
		t.Fatal(err)
	}
	clientMIC, err := mechanismMIC(key, acceptor.mechList, flags, true)
	if err != nil {
		t.Fatal(err)
	}
	token, err = encodeResponse(-1, nil, wrapped.token, clientMIC)
	if err != nil {
		t.Fatal(err)
	}
	final, err := acceptor.Step(token)
	if err != nil || !final.Done {
		t.Fatalf("client dropping 128-bit flag was refused: %v", err)
	}
	requireBytes(t, final.SessionKey, key)
	wrapped, err = decodeSPNEGO(final.Token)
	if err != nil {
		t.Fatal(err)
	}
	serverMIC, err := mechanismMIC(key, acceptor.mechList, flags, false)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, wrapped.mic, serverMIC)
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

func TestNTLMWithoutOptimisticToken(t *testing.T) {
	acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
	challenge, _ := selectedChallenge(t, acceptor, initiator, []asn1.ObjectIdentifier{ntlmOID}, nil, 1)
	authenticate, err := initiator.Step(challenge.Token)
	if err != nil {
		t.Fatal(err)
	}
	final, err := acceptor.Step(authenticate.Token)
	if err != nil || !final.Done {
		t.Fatalf("NTLM without an optimistic token failed: %v", err)
	}
	requireBytes(t, final.SessionKey, authenticate.SessionKey)
	if done, finalErr := initiator.Step(final.Token); finalErr != nil || !done.Done {
		t.Fatalf("final acceptance failed: %v", finalErr)
	}
}

func TestInitiatorRequiresNamingPairs(t *testing.T) {
	for _, omitted := range []uint16{avComputer, avDomain} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		challenge := clientChallenge(t, acceptor, initiator, offeredFlags)
		var pairs []avPair
		for _, id := range []uint16{avComputer, avDomain, avTimestamp} {
			if id != omitted {
				pairs = append(pairs, avPair{id: id, value: challenge.av[id]})
			}
		}
		info, err := encodeAV(pairs)
		if err != nil {
			t.Fatal(err)
		}
		raw := append(bytes.Clone(challenge.raw), info...)
		if _, fieldErr := putField(raw, 40, len(challenge.raw), info); fieldErr != nil {
			t.Fatal(fieldErr)
		}
		token, err := encodeResponse(1, ntlmOID, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := initiator.Step(token)
		requireFailure(t, result, err)
	}
}

func TestRequiredAuthenticateFlags(t *testing.T) {
	for _, test := range []struct {
		remove uint32
		add    uint32
	}{
		{remove: flagUnicode},
		{remove: flagNTLM},
		{remove: flagExtended},
		{remove: flagSign, add: flagKeyExch},
		{add: flagAnonymous},
	} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		challenge := clientChallenge(t, acceptor, initiator, offeredFlags)
		flags := challenge.flags&offeredFlags&^test.remove | test.add
		token, _ := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, true), challenge.av[avTimestamp], flags, true)
		result, err := acceptor.Step(token)
		requireFailure(t, result, err)
	}
	acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
	challenge := clientChallenge(t, acceptor, initiator, offeredFlags&^flagKeyExch)
	token, _ := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, true), challenge.av[avTimestamp], offeredFlags, true)
	result, err := acceptor.Step(token)
	requireFailure(t, result, err)
}

func TestUnusedSessionKeyFields(t *testing.T) {
	acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
	challenge := clientChallenge(t, acceptor, initiator, offeredFlags&^flagKeyExch)
	token, key := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, true), challenge.av[avTimestamp], offeredFlags&^flagKeyExch, true)
	wrapped, err := decodeSPNEGO(token)
	if err != nil {
		t.Fatal(err)
	}
	message, err := decodeNTLM(wrapped.token)
	if err != nil {
		t.Fatal(err)
	}
	// MS-NLMP 2.2.1.3 says these fields MUST be ignored without KEY_EXCH.
	littleEndian.PutUint16(message.raw[52:], 65535)
	littleEndian.PutUint32(message.raw[56:], 0xffffffff)
	mic, err := transcriptMIC(key, initiator.negotiate, challenge.raw, message.raw, message.micOffset)
	if err != nil {
		t.Fatal(err)
	}
	copy(message.raw[message.micOffset:], mic)
	token, err = encodeResponse(-1, nil, message.raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := acceptor.Step(token)
	if err != nil || !result.Done {
		t.Fatalf("unused session key fields affected authentication: %v", err)
	}
	requireBytes(t, result.SessionKey, key)
}

func TestSPNEGOMechanismPositions(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		mechanisms := []asn1.ObjectIdentifier{ntlmOID, ntlmOID}
		state := 1
		if !duplicate {
			mechanisms = nil
			for index := 0; index < 17; index++ {
				mechanisms = append(mechanisms, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 2, 20 + index})
			}
			mechanisms = append(mechanisms, ntlmOID)
			state = 3
		}
		token := initialWithMechanisms(t, mechanisms, nil)
		result, err := testAcceptor(t, vectorAccount).Step(token)
		if err != nil {
			t.Fatalf("valid mechanism list was refused: %v", err)
		}
		selection, err := decodeSPNEGO(result.Token)
		if err != nil || selection.state != state || !selection.mechanism.Equal(ntlmOID) {
			t.Fatalf("incorrect selection: %+v, %v", selection, err)
		}
	}
}

func TestSPNEGOExtensions(t *testing.T) {
	// RFC 4178 section 6: unrecognized extension fields are ignored.
	initial := hexBytes(t, "602106062b0601050502a0173015a00e300c060a2b06010401823702020aa503040101")
	if _, err := testInitiator(t, vectorAccount).Start(initial); err != nil {
		t.Fatalf("unknown initial extension was refused: %v", err)
	}
	initiator := testInitiator(t, vectorAccount)
	startExchange(t, testAcceptor(t, vectorAccount), initiator)
	final := hexBytes(t, "a10c300aa0030a0100a403040101")
	result, err := initiator.Step(final)
	if err != nil || !result.Done {
		t.Fatalf("unknown final extension was refused: %v", err)
	}
}

func TestTargetInfoMICFlag(t *testing.T) {
	info := testAppendAV(t, nil, avComputer, encodeUTF16("Server"))
	info = testAppendAV(t, info, avFlags, []byte{4, 0, 0, 0})
	info = testAppendAV(t, info, avEnd, nil)
	original := bytes.Clone(info)
	withMIC, err := targetInfoWithMIC(info)
	if err != nil {
		t.Fatal(err)
	}
	pairs, err := decodeAV(withMIC)
	if err != nil {
		t.Fatal(err)
	}
	if littleEndian.Uint32(pairs[avFlags]) != 6 {
		t.Fatal("initiator did not add MIC-present while preserving existing flags")
	}
	requireBytes(t, info, original)
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
