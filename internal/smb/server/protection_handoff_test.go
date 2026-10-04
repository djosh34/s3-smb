package server

import (
	"testing"
)

func TestReplacementBetweenVerificationAndDispatchRetainsReplyProtection(t *testing.T) {
	for _, test := range []struct {
		name      string
		encrypted bool
		required  bool
		denied    bool
	}{
		{name: "signed"},
		{name: "encrypted", encrypted: true, required: true},
		{name: "encrypted_when_plaintext_allowed", encrypted: true},
		{name: "bad_signature_denial", denied: true},
		{name: "plaintext_policy_denial", required: true, denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkProtectionHandoff(t, test.encrypted, test.required, test.denied)
		})
	}
}
