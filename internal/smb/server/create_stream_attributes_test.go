package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateNamedStreamDestructionPreservesBaseAttributes(t *testing.T) {
	storage := newFilesMetaStorage(t)
	client := newReadWriteClient(t, storage)
	for _, disposition := range []uint32{fileSupersede, fileOverwrite, fileOverwriteIf} {
		for _, desired := range []uint32{0, 0x100} {
			t.Run(fmt.Sprintf("disposition_%d_attributes_%x", disposition, desired), func(t *testing.T) {
				base := fmt.Sprintf("stream-base-%d-%x", disposition, desired)
				request := createRequest(base, fileCreateDisposition)
				request.FileAttributes = 0x6 // HIDDEN and SYSTEM belong to the base.
				opened := createdFile(t, client.create(t, request))
				writeCreatedFile(t, client, opened.ID, "base bytes")
				requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
				request = createRequest(base+":fork", fileCreateDisposition)
				stream := createdFile(t, client.create(t, request))
				writeCreatedFile(t, client, stream.ID, "old stream")
				requireIOStatus(t, client.close(t, stream.ID, 0), smb.StatusSuccess)
				request.Disposition, request.FileAttributes = disposition, desired
				replaced := createdFile(t, client.create(t, request))
				readCreatedFile(t, client, replaced.ID, "")
				requireIOStatus(t, client.close(t, replaced.ID, 0), smb.StatusSuccess)
				requireDeletionData(t, storage, base, "base bytes")
				selected, err := storage.Lookup(t.Context(), base)
				if err != nil || selected.Attr.Attributes != 0x26 {
					t.Fatalf("named stream changed base attributes: %+v, %v", selected, err)
				}
			})
		}
	}
}
