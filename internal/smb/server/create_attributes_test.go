package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateSupersedeReplacesAttributes(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	for _, attributes := range []uint32{0, 0x2} {
		t.Run(fmt.Sprintf("attributes_%x", attributes), func(t *testing.T) {
			name := fmt.Sprintf("supersede-%x", attributes)
			request := createRequest(name, fileCreateDisposition)
			request.FileAttributes = 0x100 // FILE_ATTRIBUTE_TEMPORARY.
			first := createdFile(t, client.create(t, request))
			if first.Attributes != request.FileAttributes {
				t.Fatalf("initial CREATE attributes %#x", first.Attributes)
			}
			requireIOStatus(t, client.close(t, first.ID, 0), smb.StatusSuccess)
			request.Disposition, request.FileAttributes = fileSupersede, attributes
			replaced := createdFile(t, client.create(t, request))
			want := attributes
			if want == 0 {
				want = 0x80 // FILE_ATTRIBUTE_NORMAL.
			}
			if replaced.Attributes != want {
				t.Fatalf("SUPERSEDE attributes %#x, want %#x", replaced.Attributes, want)
			}
			requireIOStatus(t, client.close(t, replaced.ID, 0), smb.StatusSuccess)
			reopened := createdFile(t, client.create(t, createRequest(name, fileOpen)))
			if reopened.Attributes != want {
				t.Fatalf("reopened attributes %#x, want %#x", reopened.Attributes, want)
			}
			requireIOStatus(t, client.close(t, reopened.ID, 0), smb.StatusSuccess)
		})
	}
}
