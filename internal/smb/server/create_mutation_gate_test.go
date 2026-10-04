package server

import (
	"testing"
)

func TestCreateDuringWriteWithholdsFreshReadLease(t *testing.T) {
	for _, outcome := range []string{"success", "error", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			checkCreateDuringWriteWithholdsFreshReadLease(t, outcome)
		})
	}
}
