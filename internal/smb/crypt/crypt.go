// Package crypt owns SMB 3.1.1 preauth hashing, key derivation, signing and GCM
// transforms. It must not authenticate users, parse command bodies or keep SMB
// opens. It rejects unknown algorithms, wrong key lengths and bad tags.
//
// M1 provides NewPreauth() *Preauth and NewProtector(options Options) (*Protector,
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
// M1 adds private state and these methods. Callers must use NewPreauth.
// Update computes SHA512(previous hash || exact message bytes).
// Sum returns a copy. Fork returns an independent transcript with that hash.
//
//	func (preauth *Preauth) Update(message []byte)
//	func (preauth *Preauth) Sum() PreauthHash
//	func (preauth *Preauth) Fork() *Preauth
type Preauth struct{}

// Role selects directional encryption and decryption labels in the SMB KDF.
type Role uint8

const (
	// RoleServer sends with S2C and receives with C2S keys.
	RoleServer Role = iota
	// RoleClient sends with C2S and receives with S2C keys, for the test client.
	RoleClient
)

// Options fixes one session's algorithms and derivation inputs. Cipher and
// Signing use smb's algorithm constants. Cipher zero means a signed-only session;
// Seal and Open then return an error. Random supplies nonce seed material;
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
// Seal and Open work on a whole SMB payload and the 52-byte transform header, not
// TCP framing. Encrypted messages are GCM-authenticated, not separately signed.
// Authentication happens before exposing plaintext to wire or server dispatch.
// M1 adds private state and these methods. Callers must use NewProtector.
// Sign requires a zero signature field and derives the GMAC nonce per MS-SMB2.
// Verify checks the received member in constant time.
// Seal builds a GCM transform for this session and role.
// Open validates the session, size, reserved fields, nonce and tag.
// It rejects wrong-direction keys and exposes no plaintext on failure.
//
//	func (protector *Protector) Sign(member []byte) ([16]byte, error)
//	func (protector *Protector) Verify(member []byte) error
//	func (protector *Protector) Seal(plaintext []byte) ([]byte, error)
//	func (protector *Protector) Open(transform []byte) ([]byte, error)
type Protector struct{}
