package crypt

import "crypto/sha512"

// Preauth hashes exact NEGOTIATE and SESSION_SETUP bytes, without the direct-TCP
// prefix. Each session forks the connection hash before its first setup request.
// The caller supplies messages in protocol order, not handler completion order.
// Derivation uses the completed setup-request transcript per MS-SMB2; the signed
// final SESSION_SETUP response is not fed back into its own signing key.
// Callers must serialize updates.
type Preauth struct {
	hash PreauthHash
}

// NewPreauth starts a transcript with the all-zero hash.
func NewPreauth() *Preauth {
	return &Preauth{}
}

// Update computes SHA512(previous hash || exact message bytes).
func (preauth *Preauth) Update(message []byte) {
	input := make([]byte, 0, len(preauth.hash)+len(message))
	input = append(input, preauth.hash[:]...)
	input = append(input, message...)
	preauth.hash = sha512.Sum512(input)
}

// Sum returns a copy of the current transcript hash.
func (preauth *Preauth) Sum() PreauthHash {
	return preauth.hash
}

// Fork returns an independent transcript with the current hash.
func (preauth *Preauth) Fork() *Preauth {
	return &Preauth{hash: preauth.hash}
}
