package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReplacementDrainsSynchronousAndUnpublishedAsyncWork(t *testing.T) {
	for _, command := range []wire.Command{wire.Create, wire.Read} {
		t.Run(fmt.Sprintf("command_%d", command), func(t *testing.T) {
			checkReplacementInFlight(t, command)
		})
	}
}
