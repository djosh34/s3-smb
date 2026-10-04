// Package crypt owns SMB 3.1.1 preauth hashing, key derivation, signing and GCM
// transforms. It does not authenticate users, parse command bodies or keep opens.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb"
)

// PreauthHash is the SHA-512 transcript hash. It starts at all zeroes.
type PreauthHash [64]byte

// Role selects directional encryption and decryption labels in the SMB KDF.
type Role uint8

const (
	// RoleServer sends with S2C and receives with C2S keys.
	RoleServer Role = iota
	// RoleClient sends with C2S and receives with S2C keys, for the test client.
	RoleClient
)

// Options fixes one session's algorithms and derivation inputs. Cipher and
// Signing use smb's algorithm constants. Cipher zero means a signed-only session.
// SessionKey is the 16-byte NTLM exported key, even for AES-256-GCM. Random
// supplies nonce seed material and defaults to crypto/rand.Reader. An injected
// reader must be cryptographically secure in production. Create a new protector
// with fresh derivation inputs on reconnect, never a second sender with old keys.
type Options struct {
	Random     io.Reader
	SessionKey []byte
	Preauth    PreauthHash
	SessionID  uint64
	Cipher     uint16
	Signing    uint16
	Role       Role
}

// Protector is safe for concurrent calls. Signing uses call-local state. Each
// compound member is signed independently with its own header and padding.
// Seal and Open work on a whole SMB payload and the 52-byte transform header,
// not TCP framing. Encrypted messages are GCM-authenticated, not separately signed.
// Callers must use NewProtector and must not copy a protector.
type Protector struct {
	signBlock cipher.Block
	signGMAC  cipher.AEAD
	send      cipher.AEAD
	receive   cipher.AEAD
	nonceMu   sync.Mutex
	sessionID uint64
	counter   uint64
	seed      [4]byte
}

// NewProtector derives session keys as specified in MS-SMB2 sections 3.1.4.2
// and 3.3.5.5.3. Signing keys are always 128 bits; cipher keys are 128 or 256 bits.
// Unknown algorithms, roles, incorrect NTLM key lengths and random errors fail.
func NewProtector(options Options) (*Protector, error) {
	if len(options.SessionKey) != 16 {
		return nil, fmt.Errorf("SMB NTLM session key must be 16 bytes")
	}
	if options.Role != RoleServer && options.Role != RoleClient {
		return nil, fmt.Errorf("unknown SMB protection role %d", options.Role)
	}
	if options.Signing != smb.SigningCMAC && options.Signing != smb.SigningGMAC {
		return nil, fmt.Errorf("unsupported SMB signing algorithm %d", options.Signing)
	}
	keySize := 16
	switch options.Cipher {
	case 0, smb.CipherAES128GCM:
	case smb.CipherAES256GCM:
		keySize = 32
	default:
		return nil, fmt.Errorf("unsupported SMB cipher %d", options.Cipher)
	}
	signingKey, err := deriveKey(options.SessionKey, "SMBSigningKey", options.Preauth, 16)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(signingKey)
	if err != nil {
		return nil, fmt.Errorf("create signing cipher: %w", err)
	}
	protector := &Protector{sessionID: options.SessionID}
	if options.Signing == smb.SigningCMAC {
		protector.signBlock = block
	} else {
		protector.signGMAC, err = cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create GMAC signer: %w", err)
		}
	}
	if options.Cipher == 0 {
		return protector, nil
	}
	sendLabel, receiveLabel := "SMBS2CCipherKey", "SMBC2SCipherKey"
	if options.Role == RoleClient {
		sendLabel, receiveLabel = receiveLabel, sendLabel
	}
	protector.send, err = deriveGCM(options, sendLabel, keySize)
	if err != nil {
		return nil, err
	}
	protector.receive, err = deriveGCM(options, receiveLabel, keySize)
	if err != nil {
		return nil, err
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	if _, err := io.ReadFull(random, protector.seed[:]); err != nil {
		return nil, fmt.Errorf("read GCM nonce seed: %w", err)
	}
	return protector, nil
}

func deriveGCM(options Options, label string, size int) (cipher.AEAD, error) {
	key, err := deriveKey(options.SessionKey, label, options.Preauth, size)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create encryption cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return gcm, nil
}
