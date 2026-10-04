package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestSetInfoNormalTransitionsThroughCloseAndReopen(t *testing.T) {
	storage := newFilesMetaStorage(t)
	client := newReadWriteClient(t, storage)
	opened := createdFile(t, client.create(t, createRequest("normal", fileCreateDisposition)))
	for _, attributes := range []uint32{0x80, 0x20, 0x2, 0x80, 0} {
		input, err := wire.EncodeFileBasicInformation(wire.FileBasicInformation{Attributes: attributes})
		if err != nil {
			t.Fatal(err)
		}
		body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: opened.ID, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileBasic), Input: input})
		if err != nil {
			t.Fatal(err)
		}
		requireIOStatus(t, client.exchange(t, wire.SetInfo, body, 1), smb.StatusSuccess)
		want := attributes
		if want == 0 {
			want = 0x80 // Zero input leaves the preceding NORMAL unchanged.
		}
		closed := client.close(t, opened.ID, 1)
		requireIOStatus(t, closed, smb.StatusSuccess)
		postquery, err := wire.DecodeCloseResponse(closed)
		if err != nil || postquery.Attributes != want {
			t.Fatalf("CLOSE attributes %#x, want %#x; %v", postquery.Attributes, want, err)
		}
		opened = createdFile(t, client.create(t, createRequest("normal", fileOpen)))
		if opened.Attributes != want {
			t.Fatalf("reopen attributes %#x, want %#x", opened.Attributes, want)
		}
	}
	requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
}
