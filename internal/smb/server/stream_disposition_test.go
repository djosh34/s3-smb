package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestStreamDispositionMarkAndClear(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "base bytes")
	seedDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	seedDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
	peer := newDeletionPeer(t, server)
	selected := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
	peer.disposition(t, selected, true, smb.StatusSuccess)
	peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	base := peer.open(t, "data", fileReadData, fileOpen, 0, smb.StatusSuccess)
	sibling := peer.open(t, "data:sibling:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.disposition(t, selected, false, smb.StatusSuccess)
	reopened := peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.close(t, selected)
	peer.close(t, reopened)
	peer.close(t, base)
	peer.close(t, sibling)
	requireDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	requireDeletionData(t, storage, "data", "base bytes")
	requireDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
}

func TestStreamDispositionRequiresDeleteAccess(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "base bytes")
	seedDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	peer := newDeletionPeer(t, server)
	selected := peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.disposition(t, selected, true, smb.StatusAccessDenied)
	peer.disposition(t, selected, false, smb.StatusAccessDenied)
	reopened := peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.close(t, selected)
	peer.close(t, reopened)
	requireDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	requireDeletionData(t, storage, "data", "base bytes")
}

// DELETE sharing is checked when CREATE grants access, before SET_INFO can
// use that grant. Deny-delete opens on other objects do not deny stream deletion.
func TestStreamDispositionRequiresSelectedStreamDeleteSharing(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "base bytes")
	seedDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	seedDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
	peer := newDeletionPeer(t, server)
	openWithSharing := func(path string, access, sharing uint32, want smb.Status) wire.FileID {
		t.Helper()
		response := fileCreate(peer.ctx, t, peer.client, peer.session, peer.next, wire.CreateRequest{
			Name: path, DesiredAccess: access, Disposition: fileOpen, ShareAccess: sharing, ImpersonationLevel: 2,
		})
		peer.next++
		if response.Header.Status != want {
			t.Fatalf("CREATE %s: %#x, want %#x", path, response.Header.Status, want)
		}
		if want != smb.StatusSuccess {
			return wire.FileID{}
		}
		return createdFile(t, response).ID
	}
	deny := openWithSharing("data:stream:$DATA", fileReadData, 3, smb.StatusSuccess)
	peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSharingViolation)
	peer.disposition(t, deny, true, smb.StatusAccessDenied)
	peer.close(t, deny)
	base := openWithSharing("data", fileReadData, 3, smb.StatusSuccess)
	sibling := openWithSharing("data:sibling:$DATA", fileReadData, 3, smb.StatusSuccess)
	selected := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
	openWithSharing("data:stream:$DATA", fileReadData, 3, smb.StatusSharingViolation)
	peer.disposition(t, selected, true, smb.StatusSuccess)
	peer.close(t, selected)
	requireDeletionMissing(t, storage, "data:stream:$DATA")
	requireDeletionData(t, storage, "data", "base bytes")
	requireDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
	peer.close(t, base)
	peer.close(t, sibling)
}

func TestStreamDispositionCloseAndDrop(t *testing.T) {
	for _, ending := range []string{"close", "drop"} {
		for _, another := range []bool{false, true} {
			name := ending + "/last open"
			if another {
				name = ending + "/another stream open"
			}
			t.Run(name, func(t *testing.T) {
				server := deletionServer(t)
				storage := server.options.Storage
				seedDeletionData(t, storage, "data", "base bytes")
				seedDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
				seedDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
				deleting := newDroppingDeletionPeer(t, server)
				surviving := newDeletionPeer(t, server)
				var held wire.FileID
				if another {
					held = surviving.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
				}
				selected := deleting.peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
				deleting.peer.disposition(t, selected, true, smb.StatusSuccess)
				surviving.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
				base := surviving.open(t, "data", fileReadData, fileOpen, 0, smb.StatusSuccess)
				sibling := surviving.open(t, "data:sibling:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
				if ending == "close" {
					deleting.peer.close(t, selected)
				} else {
					deleting.drop(t)
				}
				if another {
					requireDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
					surviving.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
					surviving.close(t, held)
				}
				requireDeletionMissing(t, storage, "data:stream:$DATA")
				requireDeletionData(t, storage, "data", "base bytes")
				requireDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
				// CompleteDelete must release the pending barrier after cleanup.
				recreated := surviving.open(t, "data:stream:$DATA", fileWriteData, fileOpenIf, 0, smb.StatusSuccess)
				surviving.close(t, recreated)
				requireDeletionData(t, storage, "data:stream:$DATA", "")
				surviving.close(t, base)
				surviving.close(t, sibling)
			})
		}
	}
}

func TestStreamDispositionClearsOnlyCallingOpenIntent(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "base bytes")
	seedDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	peer := newDeletionPeer(t, server)
	first := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
	second := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
	peer.disposition(t, first, true, smb.StatusSuccess)
	peer.disposition(t, second, true, smb.StatusSuccess)
	peer.disposition(t, first, false, smb.StatusSuccess)
	peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	peer.disposition(t, second, false, smb.StatusSuccess)
	reopened := peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.close(t, reopened)
	peer.disposition(t, first, true, smb.StatusSuccess)
	peer.close(t, first)
	peer.disposition(t, second, false, smb.StatusSuccess)
	peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	peer.close(t, second)
	requireDeletionMissing(t, storage, "data:stream:$DATA")
	requireDeletionData(t, storage, "data", "base bytes")
}

func TestStreamDispositionOnNonemptyDirectory(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	peer := newDeletionPeer(t, server)
	directory := peer.open(t, "directory", fileDelete, fileOpenIf, fileDirectoryFile, smb.StatusSuccess)
	seedDeletionData(t, storage, "directory/child", "child bytes")
	seedDeletionData(t, storage, "directory:stream:$DATA", "selected bytes")
	seedDeletionData(t, storage, "directory:sibling:$DATA", "sibling bytes")
	selected := peer.open(t, "directory:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
	peer.disposition(t, directory, true, smb.StatusDirectoryNotEmpty)
	peer.disposition(t, selected, true, smb.StatusSuccess)
	peer.close(t, selected)
	requireDeletionMissing(t, storage, "directory:stream:$DATA")
	requireDeletionData(t, storage, "directory/child", "child bytes")
	requireDeletionData(t, storage, "directory:sibling:$DATA", "sibling bytes")
	peer.close(t, directory)
}

func TestStreamDispositionFollowsRenamedBaseIdentity(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "base bytes")
	seedDeletionData(t, storage, "data:stream:$DATA", "selected bytes")
	seedDeletionData(t, storage, "data:sibling:$DATA", "sibling bytes")
	peer := newDeletionPeer(t, server)
	base := peer.open(t, "data", fileDelete, fileOpen, 0, smb.StatusSuccess)
	selected := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, 0, smb.StatusSuccess)
	input, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: "renamed"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{
		ID: base, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileRename), Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(peer.ctx, t, peer.client, peer.message(wire.SetInfo, body))[0]
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("SET_INFO rename: %#x", response.Header.Status)
	}
	seedDeletionData(t, storage, "data", "replacement base")
	seedDeletionData(t, storage, "data:stream:$DATA", "replacement stream")
	peer.disposition(t, selected, true, smb.StatusSuccess)
	peer.open(t, "renamed:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	peer.close(t, selected)
	requireDeletionMissing(t, storage, "renamed:stream:$DATA")
	requireDeletionData(t, storage, "renamed", "base bytes")
	requireDeletionData(t, storage, "renamed:sibling:$DATA", "sibling bytes")
	requireDeletionData(t, storage, "data", "replacement base")
	requireDeletionData(t, storage, "data:stream:$DATA", "replacement stream")
	peer.close(t, base)
}
