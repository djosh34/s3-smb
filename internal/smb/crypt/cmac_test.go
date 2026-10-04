package crypt

import (
	"crypto/aes"
	"encoding/hex"
	"strings"
	"testing"
)

func decodeHex(t testing.TB, text string) []byte {
	t.Helper()
	data, err := hex.DecodeString(strings.Join(strings.Fields(text), ""))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// RFC 4493 section 4 checks empty, complete and partial final blocks.
func TestCMACVectors(t *testing.T) {
	block, err := aes.NewCipher(decodeHex(t, "2b7e151628aed2a6abf7158809cf4f3c"))
	if err != nil {
		t.Fatal(err)
	}
	message := decodeHex(t, `
		6bc1bee22e409f96e93d7e117393172a ae2d8a571e03ac9c9eb76fac45af8e51
		30c81c46a35ce411e5fbc1191a0a52ef f69f2445df4f9b17ad2b417be66c3710`)
	vectors := []struct {
		tag    string
		length int
	}{
		{tag: "bb1d6929e95937287fa37d129b756746", length: 0},
		{tag: "070a16b46b4d4144f79bdd9dd04a287c", length: 16},
		{tag: "dfa66747de9ae63030ca32611497c827", length: 40},
		{tag: "51f0bebf7e3b9d92fc49741779363cfe", length: 64},
	}
	for _, vector := range vectors {
		got := aesCMAC(block, message[:vector.length])
		if hex.EncodeToString(got[:]) != vector.tag {
			t.Errorf("length %d: CMAC = %x, want %s", vector.length, got, vector.tag)
		}
	}
}
