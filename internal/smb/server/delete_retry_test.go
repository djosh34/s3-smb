package server

import (
	"testing"
)

func TestDeleteOnCloseRetriesRenameBetweenPathAndGuard(t *testing.T) {
	for _, stream := range []string{"", ":stream:$DATA"} {
		for _, outcome := range []string{"renamed", "replaced", "gone"} {
			t.Run(stream+"/"+outcome, func(t *testing.T) {
				checkDeletionPathRace(t, stream, outcome)
			})
		}
	}
}
