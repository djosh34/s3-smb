package server

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

const (
	createFailOpen int32 = iota + 1
	createFailTruncate
	createFailGetAttr
	createFailClose
)

type createFaultStorage struct {
	smb.Storage
	failure       atomic.Int32
	opens, closes atomic.Int64
}

func (storage *createFaultStorage) Open(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	handle, err := storage.Storage.Open(ctx, object, access)
	if err != nil {
		return handle, err
	}
	storage.opens.Add(1)
	if storage.failure.Load() == createFailOpen {
		// Even a reference returned with an error must be released.
		return handle, smb.ErrIO
	}
	return handle, nil
}

func (storage *createFaultStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.closes.Add(1)
	err := storage.Storage.Close(ctx, handle)
	if storage.failure.Load() == createFailClose {
		return errors.Join(err, smb.ErrIO)
	}
	return err
}

func (storage *createFaultStorage) Truncate(ctx context.Context, handle smb.Handle, size uint64) error {
	if storage.failure.Load() == createFailTruncate {
		return smb.ErrIO
	}
	return storage.Storage.Truncate(ctx, handle, size)
}

func (storage *createFaultStorage) GetAttr(ctx context.Context, object smb.ObjectKey) (smb.Attr, error) {
	if storage.failure.Load() == createFailGetAttr {
		return smb.Attr{}, smb.ErrIO
	}
	return storage.Storage.GetAttr(ctx, object)
}

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
