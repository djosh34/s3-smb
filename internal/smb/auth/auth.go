// Package auth owns NTLMv2 and SPNEGO authentication for one configured user.
// It must not own SMB sessions, signing keys, tree access or connection I/O.
// Tokens are bounds checked and responses compared in constant time. No NTLMv1,
// guest, anonymous or Kerberos. Reviewed ports keep their attribution.
//
// M1 provides NewAcceptor(options Options) (Acceptor, error) and
// NewInitiator(account Account, random io.Reader) (Initiator, error). Each instance
// serves one authentication exchange and is not shared by concurrent sessions.
// Initiator exists only for the raw test client, not as a public SMB client.
package auth

import (
	"io"
	"time"
)

// Account is the single allowed identity. Domain may be empty. Password and
// derived secrets must never appear in logs; User returned below is canonical.
type Account struct {
	User     string
	Domain   string
	Password string
}

// Options supplies identity and nondeterminism for reproducible MS-NLMP vectors.
// Random must be cryptographically secure in production; read errors propagate.
// Now is the server clock. ServerName is the NTLM target name, not a share name.
type Options struct {
	Random     io.Reader
	Now        func() time.Time
	Account    Account
	ServerName string
}

// Result is one handshake step. Done is true only after proof verification.
// SessionKey is the exported session key, never the password or NT hash. For
// AES-256 SMB encryption, crypt derives a 256-bit key from this exported key.
// User and SessionKey are empty before completion. Failed steps return errors,
// never a partial authenticated result.
type Result struct {
	Token      []byte
	SessionKey []byte
	User       string
	Done       bool
}

// Acceptor handles SPNEGO wrapping and the NTLMv2 server exchange. Bad token
// syntax and bad credentials return errors for SESSION_SETUP to map to failure.
// InitialToken lists only NTLMSSP's OID. Authenticate verifies MIC where required.
type Acceptor interface {
	// InitialToken builds the NEGOTIATE security blob without starting an exchange.
	InitialToken() ([]byte, error)
	// Step accepts the client's negotiate, then authenticate token, in that order.
	Step(token []byte) (Result, error)
}

// Initiator supplies the minimal NTLMv2 exchange for tests. It uses the same
// Result contract and server proof/key validation as the protocol requires.
type Initiator interface {
	// Start consumes the server's SPNEGO mechanism list and emits a negotiate token.
	Start(serverToken []byte) (Result, error)
	// Step consumes a challenge or final token and emits the next token if any.
	Step(serverToken []byte) (Result, error)
}
