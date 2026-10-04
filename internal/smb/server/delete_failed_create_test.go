package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestFailedCreateCompletesDeleteOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "close failure", "remove failure", "changed inode", "remove cancelled"} {
		t.Run(outcome, func(t *testing.T) { checkFailedCreateDeletion(t, outcome) })
	}
}

func TestDeletionWithActuallyCancelledContext(t *testing.T) {
	for _, boundary := range []string{"bulk cleanup", "cleanup action"} {
		t.Run(boundary, func(t *testing.T) {
			server := deletionServer(t)
			storage := server.options.Storage
			seedDeletionData(t, storage, "data", "base")
			seedDeletionData(t, storage, "data:stream:$DATA", "stream")
			peer := newDeletionPeer(t, server)
			id := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
			request := deletionRequest(server, peer)
			action, status := request.Opens.Close(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, request.Binding())
			if status != smb.StatusSuccess || !action.Remove {
				t.Fatalf("close = %+v, %#x", action, status)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if boundary == "bulk cleanup" {
				if err := server.cleanup(ctx, []state.CloseAction{action}); err != nil {
					t.Fatal(err)
				}
				requireDeletionMissing(t, storage, "data:stream:$DATA")
			} else {
				if err := server.cleanupAction(ctx, action); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled action = %v", err)
				}
				requireDeletionData(t, storage, "data:stream:$DATA", "stream")
			}
			requireDeletionData(t, storage, "data", "base")
			id = peer.open(t, "data:stream:$DATA", fileReadData, fileOpenIf, 0, smb.StatusSuccess)
			peer.close(t, id)
		})
	}
}
