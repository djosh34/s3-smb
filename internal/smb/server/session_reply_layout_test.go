package server

import (
	"testing"
)

// Reply offsets follow MS-SMB2 2.2.6 (SESSION_SETUP), 2.2.8 (LOGOFF),
// 2.2.10 (TREE_CONNECT) and 2.2.12 (TREE_DISCONNECT). Only requests and the
// prerequisite NEGOTIATE reply use wire codecs.
func TestSessionAndTreeReplyByteLayouts(t *testing.T) {
	for _, mode := range []struct {
		name   string
		policy EncryptionPolicy
	}{
		{"signed_plaintext", AllowPlaintext},
		{"encrypted", RequireEncryption},
	} {
		t.Run(mode.name, func(t *testing.T) {
			options := testOptions(t)
			options.Encryption = mode.policy
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx, sessionID, protector := sessionLayoutLogin(t, server)
			checkTreeAndCleanupLayouts(ctx, t, client, protector, sessionID, mode.policy == RequireEncryption)
		})
	}
}
