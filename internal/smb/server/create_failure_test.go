package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestCreateFailureAbortsAndClosesReferences(t *testing.T) {
	for _, failure := range []int32{createFailOpen, createFailTruncate, createFailGetAttr} {
		t.Run(fmt.Sprintf("failure_%d", failure), func(t *testing.T) {
			storage := &createFaultStorage{Storage: newFilesMetaStorage(t)}
			client := newReadWriteClient(t, storage)
			created := createdFile(t, client.create(t, createRequest("failure", fileCreateDisposition)))
			writeCreatedFile(t, client, created.ID, "old payload")
			requireIOStatus(t, client.close(t, created.ID, 0), smb.StatusSuccess)
			storage.failure.Store(failure)
			request := createRequest("failure", fileSupersede)
			request.ShareAccess = 0
			requireIOStatus(t, client.create(t, request), smb.StatusIODeviceError)
			if storage.opens.Load() != storage.closes.Load() {
				t.Fatalf("leaked storage references: opens %d, closes %d", storage.opens.Load(), storage.closes.Load())
			}
			storage.failure.Store(0)
			// A leaked reservation would reject this exclusive retry.
			request.Disposition = fileOpen
			retry := createdFile(t, client.create(t, request))
			if failure != createFailGetAttr {
				readCreatedFile(t, client, retry.ID, "old payload")
			}
			requireIOStatus(t, client.close(t, retry.ID, 0), smb.StatusSuccess)
		})
	}
}

func TestCreateFailedCommitClosesAndAborts(t *testing.T) {
	storage := &createFaultStorage{Storage: newFilesMetaStorage(t)}
	client := newReadWriteClient(t, storage)
	request := createRequest("", fileOpen)
	request.Options, request.ShareAccess = fileDeleteOnClose, 0
	// Root has no deletion name, so the table refuses the grant after Open.
	requireIOStatus(t, client.create(t, request), smb.StatusInvalidParameter)
	if storage.opens.Load() != 1 || storage.closes.Load() != 1 {
		t.Fatalf("failed grant references: opens %d, closes %d", storage.opens.Load(), storage.closes.Load())
	}
	request.Options = fileDirectoryFile
	retry := createdFile(t, client.create(t, request))
	requireIOStatus(t, client.close(t, retry.ID, 0), smb.StatusSuccess)
}

func TestCloseErrorsStillReleaseGrantAndStorage(t *testing.T) {
	for _, failure := range []int32{createFailGetAttr, createFailClose} {
		t.Run(fmt.Sprintf("failure_%d", failure), func(t *testing.T) {
			storage := &createFaultStorage{Storage: newFilesMetaStorage(t)}
			client := newReadWriteClient(t, storage)
			created := createdFile(t, client.create(t, createRequest("close-failure", fileCreateDisposition)))
			storage.failure.Store(failure)
			requireIOStatus(t, client.close(t, created.ID, 1), smb.StatusIODeviceError)
			if storage.opens.Load() != storage.closes.Load() {
				t.Fatal("CLOSE error leaked its storage reference")
			}
			_, status := client.server.options.State.Find(state.FileID(created.ID), state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID})
			if status != smb.StatusFileClosed {
				t.Fatalf("CLOSE error retained grant: %#x", status)
			}
			storage.failure.Store(0)
		})
	}
}
