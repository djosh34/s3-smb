package crypt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// deriveKey implements SP800-108 counter mode with HMAC-SHA256, r=32 and
// L=128 or 256. MS-SMB2 includes a terminating NUL in the label, in addition
// to the KDF's separator. One PRF block supplies either supported key length.
func deriveKey(sessionKey []byte, label string, context PreauthHash, size int) ([]byte, error) {
	if size != 16 && size != 32 {
		return nil, fmt.Errorf("unsupported derived key size %d", size)
	}
	input := []byte{0, 0, 0, 1}
	input = append(input, label...)
	input = append(input, 0, 0)
	input = append(input, context[:]...)
	var length [4]byte
	if size == 16 {
		binary.BigEndian.PutUint32(length[:], 128)
	} else {
		binary.BigEndian.PutUint32(length[:], 256)
	}
	input = append(input, length[:]...)
	mac := hmac.New(sha256.New, sessionKey)
	if _, err := mac.Write(input); err != nil {
		return nil, fmt.Errorf("derive SMB key: %w", err)
	}
	return mac.Sum(nil)[:size], nil
}
