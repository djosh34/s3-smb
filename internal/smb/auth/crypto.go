// NTLMv2 formulas ported from macos-fuse-t/go-smb2,
// commit 277a9300411249a881a05f7a910f5a83ae3395f2, originally Hiroshi Ioka's go-smb2.
// Modified for s3-smb, 2026. See docs/vendored.md.
// See LICENSE and Attributions.txt in this directory for the upstream notices.
package auth

import (
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // MS-NLMP requires MD5 for NTLMv2 proofs and MICs.
	"crypto/rc4" //nolint:gosec // MS-NLMP requires RC4 for exported session key exchange.
	"hash"
	"strings"

	"golang.org/x/crypto/md4" //nolint:gosec,staticcheck // MS-NLMP requires MD4 to derive the NT password hash.
)

func hashParts(h hash.Hash, parts ...[]byte) ([]byte, error) {
	for _, part := range parts {
		if _, err := h.Write(part); err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func responseKey(account Account, user, domain string) ([]byte, error) {
	ntHash, err := hashParts(md4.New(), encodeUTF16(account.Password)) //nolint:gosec // MS-NLMP requires MD4 to derive the NT password hash.
	if err != nil {
		return nil, err
	}
	return ntlmHMAC(ntHash, encodeUTF16(strings.ToUpper(user)), encodeUTF16(domain))
}

func ntlmHMAC(key []byte, parts ...[]byte) ([]byte, error) {
	return hashParts(hmac.New(md5.New, key), parts...)
}

func exchangeKey(key, input []byte) ([]byte, error) {
	cipher, err := rc4.NewCipher(key) //nolint:gosec // MS-NLMP requires RC4 for exported session key exchange.
	if err != nil {
		return nil, err
	}
	output := make([]byte, len(input))
	cipher.XORKeyStream(output, input)
	return output, nil
}

func responseBlob(timestamp, nonce, targetInfo []byte) []byte {
	blob := make([]byte, 28+len(targetInfo)+4)
	blob[0], blob[1] = 1, 1
	copy(blob[8:16], timestamp)
	copy(blob[16:24], nonce)
	copy(blob[28:], targetInfo)
	return blob
}

func transcriptMIC(key, negotiate, challenge, authenticate []byte, offset int) ([]byte, error) {
	unsigned := append([]byte(nil), authenticate...)
	clear(unsigned[offset : offset+16])
	return ntlmHMAC(key, negotiate, challenge, unsigned)
}

// SPNEGO's optional mechListMIC uses NTLM's connection-oriented signature,
// sequence zero and separate directional signing/sealing keys. These are not
// SMB signing or encryption keys, which belong to crypt.
func mechanismMIC(key, mechList []byte, flags uint32, client bool) ([]byte, error) {
	direction := "server-to-client"
	if client {
		direction = "client-to-server"
	}
	signingKey, err := hashParts(md5.New(), key, []byte("session key to "+direction+" signing key magic constant\x00")) //nolint:gosec // MS-NLMP defines MD5-derived NTLM mechanism signing keys.
	if err != nil {
		return nil, err
	}
	var sequence [4]byte
	checksum, err := ntlmHMAC(signingKey, sequence[:], mechList)
	if err != nil {
		return nil, err
	}
	checksum = checksum[:8]
	if flags&flagKeyExch != 0 {
		sealingInput := key
		if flags&flag128 == 0 {
			length := 5
			if flags&flag56 != 0 {
				length = 7
			}
			sealingInput = key[:length]
		}
		sealingKey, err := hashParts(md5.New(), sealingInput, []byte("session key to "+direction+" sealing key magic constant\x00")) //nolint:gosec // MS-NLMP defines MD5-derived NTLM mechanism sealing keys.
		if err != nil {
			return nil, err
		}
		checksum, err = exchangeKey(sealingKey, checksum)
		if err != nil {
			return nil, err
		}
	}
	signature := make([]byte, 16)
	littleEndian.PutUint32(signature, 1)
	copy(signature[4:12], checksum)
	return signature, nil
}
