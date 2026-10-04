package server

import (
	"testing"
)

// Retire an identity after dispatch has retained its immutable request snapshot.
// The ACK transaction must revalidate that snapshot before changing lease state.
func TestLeaseAckRevalidatesIdentityBeforeMutation(t *testing.T) {
	for _, mode := range []string{"removed session", "inactive session", "removed tree", "canceled"} {
		t.Run(mode, func(t *testing.T) { checkLeaseAckIdentityGuard(t, mode) })
	}
}
