// Package auth owns NTLMv2 and SPNEGO authentication for one configured user.
// It must not own SMB sessions, signing keys, tree access or connection I/O.
// Tokens are bounds checked and responses compared in constant time. No NTLMv1,
// guest, anonymous or Kerberos. Reviewed ports keep their attribution.
//
// M1 provides NewAcceptor(options Options) (*Acceptor, error) and
// NewInitiator(account Account, random io.Reader) (*Initiator, error). Each instance
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

// Result is one handshake step. Done means the exchange is complete. Acceptor
// returns User and SessionKey only after verifying the client's proof. Initiator
// supplies its key when emitting Authenticate, before Done, so the test client
// can verify the final SESSION_SETUP signature before accepting the session.
// SessionKey is the exported key, never the password or NT hash. crypt derives
// the cipher key from it. Failed steps return errors, never an authenticated result.
type Result struct {
	User       string
	Token      []byte
	SessionKey []byte
	Done       bool
}

// Acceptor handles SPNEGO wrapping and the NTLMv2 server exchange. Bad token
// syntax and bad credentials return errors for SESSION_SETUP to map to failure.
// InitialToken lists only NTLMSSP's OID. Authenticate verifies MIC where required.
// M1 adds private state and these methods. Callers must use NewAcceptor.
// InitialToken builds the NEGOTIATE blob without starting an exchange.
// Step accepts the negotiate token, then the authenticate token.
//
//	func (acceptor *Acceptor) InitialToken() ([]byte, error)
//	func (acceptor *Acceptor) Step(token []byte) (Result, error)
type Acceptor struct{}

// Initiator supplies the minimal NTLMv2 exchange for tests. It validates SPNEGO
// acceptance; the SMB test client separately verifies the final setup signature.
// M1 adds private state and these methods. Callers must use NewInitiator.
// Start consumes the mechanism list and emits a negotiate token.
// Step consumes a challenge or final token and emits the next token if needed.
//
//	func (initiator *Initiator) Start(serverToken []byte) (Result, error)
//	func (initiator *Initiator) Step(serverToken []byte) (Result, error)
type Initiator struct{}
