package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateEveryDispositionRealAdapter(t *testing.T) {
	for disposition := fileSupersede; disposition <= fileOverwriteIf; disposition++ {
		for _, exists := range []bool{false, true} {
			for _, replacement := range []string{"", "new"} {
				name := fmt.Sprintf("disposition_%d_exists_%t_replacement_%q", disposition, exists, replacement)
				t.Run(name, func(t *testing.T) {
					client := newReadWriteClient(t, newFilesMetaStorage(t))
					checkCreateDisposition(t, client, disposition, exists, replacement)
				})
			}
		}
	}
}

func TestCreateGenericAllAndMaximumAllowWrites(t *testing.T) {
	for _, desired := range []uint32{0x10000000, 0x02000000} {
		t.Run(fmt.Sprintf("access_%x", desired), func(t *testing.T) {
			client := newReadWriteClient(t, newFilesMetaStorage(t))
			request := createRequest("writable", fileCreateDisposition)
			request.DesiredAccess = desired
			created := createdFile(t, client.create(t, request))
			writeCreatedFile(t, client, created.ID, "full access")
			readCreatedFile(t, client, created.ID, "full access")
			requireIOStatus(t, client.close(t, created.ID, 0), smb.StatusSuccess)
		})
	}
}

func TestCreateSharingFailurePreservesContents(t *testing.T) {
	for _, disposition := range []uint32{fileSupersede, fileOverwrite, fileOverwriteIf} {
		t.Run(fmt.Sprintf("disposition_%d", disposition), func(t *testing.T) {
			client := newReadWriteClient(t, newFilesMetaStorage(t))
			first := createdFile(t, client.create(t, createRequest("shared", fileCreateDisposition)))
			writeCreatedFile(t, client, first.ID, "old payload")
			requireIOStatus(t, client.close(t, first.ID, 0), smb.StatusSuccess)
			readerRequest := createRequest("shared", fileOpen)
			readerRequest.DesiredAccess, readerRequest.ShareAccess = fileReadData, 1
			reader := createdFile(t, client.create(t, readerRequest))
			requireIOStatus(t, client.create(t, createRequest("shared", disposition)), smb.StatusSharingViolation)
			readCreatedFile(t, client, reader.ID, "old payload")
			requireIOStatus(t, client.close(t, reader.ID, 0), smb.StatusSuccess)
			retry := createdFile(t, client.create(t, createRequest("shared", disposition)))
			readCreatedFile(t, client, retry.ID, "")
			requireIOStatus(t, client.close(t, retry.ID, 0), smb.StatusSuccess)
		})
	}
}
