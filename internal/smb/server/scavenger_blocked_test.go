package server

import (
	"testing"
)

func TestScavengerExpiresWhileCleanupIsBlocked(t *testing.T) {
	for _, block := range []string{"parent", "close", "open use"} {
		t.Run(block, func(t *testing.T) { checkExpiryWhileCleanupBlocked(t, block) })
	}
}
