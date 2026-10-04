package server

import (
	"testing"
)

func TestAllocationCannotUndoConcurrentEOFShrink(t *testing.T) {
	for _, path := range []string{"data", "data:fork"} {
		t.Run(path, func(t *testing.T) { checkAllocationAfterEOFShrink(t, path) })
	}
}
