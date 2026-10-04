package server

import (
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

func TestDeletePendingRefusesSeededHardlinksWithoutRemovingEitherName(t *testing.T) {
	for _, ending := range []string{"close", "logoff"} {
		t.Run(ending, func(t *testing.T) { checkSeededHardlinkDeletion(t, ending) })
	}
}

func checkSeededHardlinkDeletion(t *testing.T, ending string) {
	t.Helper()
	storage, metadata := newFilesMetaStorageWithMetadata(t, 0)
	first := seedDeletionData(t, storage, "a", "keep both links")
	var attr meta.Attr
	if errno := metadata.Link(meta.WrapWithoutCancel(t.Context(), 0, smbfs.UID, []uint32{smbfs.GID}), meta.Ino(first.Object.Inode), meta.RootInode, "b", &attr); errno != 0 {
		t.Fatal(errno)
	}
	second, err := storage.Lookup(t.Context(), "b")
	if err != nil || !second.Exists || second.Object != first.Object || attr.Nlink != 2 {
		t.Fatalf("seeded second link = %+v, nlink %d, %v", second, attr.Nlink, err)
	}
	if path, pathErr := storage.PathOf(t.Context(), first.Object.Inode); !errors.Is(pathErr, smb.ErrNameNotFound) {
		t.Fatalf("PathOf accepted multiple names: %q, %v", path, pathErr)
	}
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	peer := newDeletionPeer(t, server)
	deleted := peer.open(t, "a", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	last := peer.open(t, "b", fileReadData, fileOpen, 0, smb.StatusSuccess)
	input, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: "third"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: deleted, InfoType: wire.InfoFile, InfoClass: 11, Input: input}) // FileLinkInformation.
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(peer.ctx, t, peer.client, peer.message(wire.SetInfo, body))[0]
	if response.Header.Status != smb.StatusNotSupported {
		t.Fatalf("SMB FileLink = %#x", response.Header.Status)
	}
	peer.close(t, deleted)
	peer.open(t, "b", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	if ending == "close" {
		peer.close(t, last)
	} else {
		endDeletionSession(t, server, peer, "logoff")
	}
	requireDeletionData(t, storage, "a", "keep both links")
	requireDeletionData(t, storage, "b", "keep both links")
	requireDeletionMissing(t, storage, "third")
	other := newDeletionPeer(t, server)
	id := other.open(t, "b", fileReadData, fileOpen, 0, smb.StatusSuccess)
	other.close(t, id)
}
