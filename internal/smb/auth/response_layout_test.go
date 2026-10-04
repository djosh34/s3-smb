package auth

import (
	"bytes"
	"testing"
)

func TestAcceptorRetainsOfferedSealFlag(t *testing.T) {
	for _, flags := range []uint32{offeredFlags | flagSeal, offeredFlags&^flagSign | flagSeal} {
		acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
		challenge := clientChallenge(t, acceptor, initiator, flags)
		if challenge.flags&(flagSeal|flagKeyExch) != flagSeal|flagKeyExch {
			t.Fatal("challenge dropped NTLM sealing or its key exchange")
		}
		token, key := clientAuthenticate(t, initiator, challenge, clientTargetInfo(t, challenge, true), challenge.av[avTimestamp], challenge.flags&(offeredFlags|flagSeal), true)
		result, err := acceptor.Step(token)
		if err != nil || !result.Done {
			t.Fatalf("sealing authentication failed: %v", err)
		}
		requireBytes(t, result.SessionKey, key)
	}
}

func TestClientChallengeEndsAtMsvAvEOL(t *testing.T) {
	// MS-NLMP 2.2.2.7 ends NTLMv2_CLIENT_CHALLENGE at AvPairs. The
	// additional zero word used by the 3.3.2 response algorithm is not a
	// required field of that structure. Proof and MIC cover the actual bytes.
	acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
	challenge := clientChallenge(t, acceptor, initiator, offeredFlags&^flagKeyExch)
	info := clientTargetInfo(t, challenge, true)
	blob := make([]byte, 28+len(info))
	blob[0], blob[1] = 1, 1
	copy(blob[8:16], challenge.av[avTimestamp])
	copy(blob[16:24], bytes.Repeat([]byte{0xaa}, 8))
	copy(blob[28:], info)
	responseKey, err := responseKey(vectorAccount, vectorAccount.User, vectorAccount.Domain)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ntlmHMAC(responseKey, challenge.raw[24:32], blob)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ntlmHMAC(responseKey, proof)
	if err != nil {
		t.Fatal(err)
	}
	message, err := initiator.makeAuthenticate(challenge.flags&offeredFlags, append(proof, blob...), nil)
	if err != nil {
		t.Fatal(err)
	}
	mic, err := transcriptMIC(key, initiator.negotiate, challenge.raw, message, 72)
	if err != nil {
		t.Fatal(err)
	}
	copy(message[72:], mic)
	token, err := encodeResponse(-1, nil, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := acceptor.Step(token)
	if err != nil || !result.Done {
		t.Fatalf("challenge without extra trailer was refused: %v", err)
	}
	requireBytes(t, result.SessionKey, key)
}

func TestResponseAVRejectsExtraOrNonzeroTrailer(t *testing.T) {
	for _, tail := range [][]byte{{1, 0, 0, 0}, make([]byte, 8)} {
		data := append([]byte{0, 0, 0, 0}, tail...)
		if _, err := decodeResponseAV(data); err == nil {
			t.Fatal("invalid response trailer was accepted")
		}
	}
}
