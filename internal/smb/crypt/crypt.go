// Package crypt owns SMB 3.1.1 preauth hashing, key derivation, signing and GCM
// transforms. It must not authenticate users, parse command bodies or keep SMB
// opens. It rejects unknown algorithms, wrong key lengths and bad tags.
//
// M1 provides NewPreauth() Preauth and NewProtector(options Options) (Protector,
// error). MS-SMB2 vectors cover SHA-512, KDF, CMAC, GMAC and both GCM key lengths.
package crypt

import "io"

// PreauthHash is the SHA-512 transcript hash. It starts at all zeroes.
type PreauthHash [64]byte

// Preauth hashes exact NEGOTIATE and SESSION_SETUP bytes, without the direct-TCP
// prefix. Each session forks the connection hash before its first setup request.
// The server supplies messages in protocol order, not handler completion order.
// Derivation uses the completed setup-request transcript per MS-SMB2; the signed
// final SESSION_SETUP response is not fed back into its own signing key.
type Preauth interface {
	// Update computes SHA512(previous hash || exact message bytes).
	Update(message []byte)
	// Sum returns a copy of the current transcript hash.
	Sum() PreauthHash
	// Fork returns an independent transcript with the same current hash.
	Fork() Preauth
}

// Role selects directional encryption/decryption labels in the SMB KDF.
type Role uint8

const (
	// RoleServer sends with S2C and receives with C2S keys.
	RoleServer Role = iota
	// RoleClient sends with C2S and receives with S2C keys, for the test client.
	RoleClient
)

// Options fixes one session's algorithms and derivation inputs. Cipher and
// Signing use smb's algorithm constants. Random supplies nonce seed material;
// counters ensure no nonce reuse, including concurrent sends and async replies.
// Counter exhaustion returns an error and requires closing the session.
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
// Seal/Open work on a whole SMB payload and the 52-byte transform header, not
// TCP framing. Encrypted messages are GCM-authenticated, not separately signed.
// Authentication happens before exposing plaintext to wire or server dispatch.
type Protector interface {
	// Sign returns a 16-byte signature for a member whose signature field is zero.
	// It extracts MessageId and direction for the GMAC nonce per MS-SMB2.
	Sign(member []byte) ([16]byte, error)
	// Verify checks the exact received member in constant time.
	Verify(member []byte) error
	// Seal builds an authenticated GCM transform for this SessionID and role.
	Seal(plaintext []byte) ([]byte, error)
	// Open validates SessionID, original size, reserved fields, nonce and tag.
	// It rejects wrong-direction keys and returns no plaintext on failure.
	Open(transform []byte) ([]byte, error)
}
