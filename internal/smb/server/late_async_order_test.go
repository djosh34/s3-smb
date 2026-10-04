package server

import (
	"testing"
)

func TestLateAsyncCompletionsKeepTheirOwnIdentity(t *testing.T) {
	for _, simultaneous := range []bool{false, true} {
		name := "reverse order"
		if simultaneous {
			name = "simultaneous"
		}
		t.Run(name, func(t *testing.T) { testLateAsyncOrder(t, simultaneous) })
	}
}
