package server

import (
	"testing"
)

func TestDeletePendingRefusesSeededHardlinksWithoutRemovingEitherName(t *testing.T) {
	for _, ending := range []string{"close", "logoff"} {
		t.Run(ending, func(t *testing.T) { checkSeededHardlinkDeletion(t, ending) })
	}
}
