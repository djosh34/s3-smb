package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDroppedStreamDeletionKeepsOtherConnectionHandles(t *testing.T) {
	for _, other := range []string{"data", "data:stream:$DATA"} {
		t.Run(other, func(t *testing.T) {
			server := deletionServer(t)
			storage := server.options.Storage
			seedDeletionData(t, storage, "data", "base data")
			seedDeletionData(t, storage, "data:stream:$DATA", "stream data")
			seedDeletionData(t, storage, "data:other:$DATA", "other stream")
			dropping := newDroppingDeletionPeer(t, server)
			surviving := newDeletionPeer(t, server)
			held := surviving.open(t, other, fileReadData, fileOpen, 0, smb.StatusSuccess)
			dropping.peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
			dropping.drop(t)
			if other == "data:stream:$DATA" {
				requireDeletionData(t, storage, other, "stream data")
				surviving.open(t, other, fileReadData, fileOpen, 0, smb.StatusDeletePending)
			} else {
				requireDeletionMissing(t, storage, "data:stream:$DATA")
			}
			surviving.close(t, held)
			requireDeletionMissing(t, storage, "data:stream:$DATA")
			requireDeletionData(t, storage, "data", "base data")
			requireDeletionData(t, storage, "data:other:$DATA", "other stream")
		})
	}
}
