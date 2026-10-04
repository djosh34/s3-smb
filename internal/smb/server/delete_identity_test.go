package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

// A storage-side rename can change a name even while the server holds its
// namespace guard. The real adapter must check the inode again inside Remove.
type replacedDeletionStorage struct {
	smb.Storage
	replacement smb.RenameRequest
}

func (storage *replacedDeletionStorage) Remove(ctx context.Context, name smb.Name, expect smb.Inode) error {
	if err := storage.Rename(ctx, storage.replacement); err != nil {
		return err
	}
	return storage.Storage.Remove(ctx, name, expect)
}

func TestDeleteOnCloseLeavesChangedIdentityAlone(t *testing.T) {
	for _, stream := range []string{"", ":stream:$DATA"} {
		t.Run(stream, func(t *testing.T) {
			storage := newFilesMetaStorage(t)
			selected := seedDeletionData(t, storage, "selected", "old base")
			replacement := seedDeletionData(t, storage, "replacement", "new base")
			if stream != "" {
				seedDeletionData(t, storage, "selected"+stream, "old stream")
				seedDeletionData(t, storage, "replacement"+stream, "new stream")
			}
			wrapped := &replacedDeletionStorage{Storage: storage, replacement: smb.RenameRequest{
				Source: replacement.Name, SourceInode: replacement.Object.Inode,
				Destination: selected.Name, DestinationInode: selected.Object.Inode, Replace: true,
			}}
			options := testOptions(t)
			options.Storage = wrapped
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			peer := newDeletionPeer(t, server)
			id := peer.open(t, "selected"+stream, fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
			peer.closeExpect(t, id, smb.StatusObjectNameNotFound)
			requireDeletionData(t, storage, "selected", "new base")
			if stream != "" {
				requireDeletionData(t, storage, "selected"+stream, "new stream")
			}
			id = peer.open(t, "selected"+stream, fileReadData, fileOpen, 0, smb.StatusSuccess)
			peer.close(t, id)
		})
	}
}
