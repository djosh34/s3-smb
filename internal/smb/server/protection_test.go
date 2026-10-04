package server

import (
	"fmt"
	"testing"
)

func TestFinalSessionSetupMatchesTranscript(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	rawLogin(t, server)
}

func TestPlaintextCompoundVerifiesEveryMemberBeforeDispatch(t *testing.T) {
	for _, tamper := range []int{-1, 0, 1, 2} {
		t.Run(fmt.Sprintf("member_%d", tamper), func(t *testing.T) { checkPlainCompound(t, tamper) })
	}
}

func TestEncryptionRejectsChangedTagAndPlaintextBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"tag", "ciphertext", "session", "plaintext", "valid"} {
		t.Run(mode, func(t *testing.T) { checkEncryptionInput(t, mode) })
	}
}
