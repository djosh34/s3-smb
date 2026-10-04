package smbtest

import "github.com/djosh34/s3-smb/internal/smb/auth"

// LoginOptions is the M2 handshake contract (#188), not part of the M1 client.
// Share names the only disk share. The client derives the UNC host from its
// connection. Cipher zero requests signed plaintext; a GCM cipher requests
// encryption. Cipher and signing values use smb constants. Tests send raw
// messages to offer unsupported values.
type LoginOptions struct {
	Share             string
	Account           auth.Account
	ClientGUID        [16]byte
	PreviousSessionID uint64
	Cipher            uint16
	Signing           uint16
}

// Session is the identity and credit result of the M2 handshake. M2 adds Login
// to Client using auth.Initiator and a crypt.RoleClient protector. Login verifies
// the final setup signature before accepting the session. Send and Receive then
// sign or encrypt traffic as required. M5 reconnect uses a new client with
// PreviousSessionID, followed by DH2C and RqLs with the retained identities.
//
//	func (client *Client) Login(ctx context.Context, options LoginOptions) (Session, error)
type Session struct {
	SessionID uint64
	TreeID    uint32
	Credits   uint16
	Cipher    uint16
	Signing   uint16
}
