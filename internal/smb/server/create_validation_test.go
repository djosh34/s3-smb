package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateDeniedDeleteOnClosePreservesObject(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	for _, disposition := range []uint32{fileCreateDisposition, fileOpenIf, fileOverwrite, fileOverwriteIf, fileSupersede} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("disposition_%d_exists_%t", disposition, exists), func(t *testing.T) {
				name := fmt.Sprintf("delete-%d-%t", disposition, exists)
				if exists {
					first := createdFile(t, client.create(t, createRequest(name, fileCreateDisposition)))
					writeCreatedFile(t, client, first.ID, "old payload")
					requireIOStatus(t, client.close(t, first.ID, 0), smb.StatusSuccess)
				}
				request := createRequest(name, disposition)
				request.DesiredAccess = genericWrite
				request.Options = fileDeleteOnClose
				requireIOStatus(t, client.create(t, request), smb.StatusAccessDenied)
				resolved, err := client.server.options.Storage.Lookup(t.Context(), name)
				if err != nil || resolved.Exists != exists {
					t.Fatalf("denied CREATE changed existence: %+v, %v", resolved, err)
				}
				if exists {
					reopened := createdFile(t, client.create(t, createRequest(name, fileOpen)))
					readCreatedFile(t, client, reopened.ID, "old payload")
					requireIOStatus(t, client.close(t, reopened.ID, 0), smb.StatusSuccess)
				}
			})
		}
	}
}

func TestCreateDirectoryDestructiveDispositionLeavesNamespace(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	for _, disposition := range []uint32{fileSupersede, fileOverwrite, fileOverwriteIf} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("disposition_%d_exists_%t", disposition, exists), func(t *testing.T) {
				name := fmt.Sprintf("directory-%d-%t", disposition, exists)
				if exists {
					request := createRequest(name, fileCreateDisposition)
					request.Options = fileDirectoryFile
					first := createdFile(t, client.create(t, request))
					requireIOStatus(t, client.close(t, first.ID, 0), smb.StatusSuccess)
				}
				before, err := client.server.options.Storage.Lookup(t.Context(), name)
				if err != nil {
					t.Fatal(err)
				}
				request := createRequest(name, disposition)
				request.Options = fileDirectoryFile
				requireIOStatus(t, client.create(t, request), smb.StatusInvalidParameter)
				after, err := client.server.options.Storage.Lookup(t.Context(), name)
				if err != nil || after != before {
					t.Fatalf("invalid CREATE changed directory: before %+v, after %+v, %v", before, after, err)
				}
			})
		}
	}
}
