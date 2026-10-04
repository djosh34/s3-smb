package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateDestructiveDispositionRequiresAccess(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	created := createdFile(t, client.create(t, createRequest("protected", fileCreateDisposition)))
	writeCreatedFile(t, client, created.ID, "old payload")
	for _, test := range []struct {
		disposition, access uint32
	}{
		{fileSupersede, fileReadData | fileWriteData},
		{fileOverwrite, fileReadData},
		{fileOverwriteIf, fileAppendData},
	} {
		t.Run(fmt.Sprintf("disposition_%d_access_%x", test.disposition, test.access), func(t *testing.T) {
			request := createRequest("protected", test.disposition)
			request.DesiredAccess = test.access
			requireIOStatus(t, client.create(t, request), smb.StatusAccessDenied)
			readCreatedFile(t, client, created.ID, "old payload")
		})
	}
	requireIOStatus(t, client.close(t, created.ID, 0), smb.StatusSuccess)
}
