package server

import (
	"testing"
)

func TestNamespaceCancellationWhileWaitingForGuard(t *testing.T) {
	for _, operation := range []string{"rename", "disposition"} {
		for _, held := range []string{"first", "second"} {
			t.Run(operation+"/"+held, func(t *testing.T) {
				checkNamespaceGuardCancellation(t, operation, held)
			})
		}
	}
}
