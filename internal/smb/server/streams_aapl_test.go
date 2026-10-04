package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestAAPLEmptyStreamFirstQuery(t *testing.T) {
	c := newStreamClient(t)
	base := c.create(t, streamRequest("data", fileCreateDisposition), smb.StatusSuccess).ID
	stream := c.create(t, streamRequest("data:AFP_Resource", fileCreateDisposition), smb.StatusSuccess).ID
	c.close(t, stream)
	query, err := wire.EncodeAAPLQuery(wire.AAPLQuery{Requested: 2})
	if err != nil {
		t.Fatal(err)
	}
	// The first AAPL query must take effect on this CREATE, not the next one.
	request := streamRequest("data:AFP_Resource", fileOpen)
	request.Contexts = []wire.CreateContext{query}
	c.create(t, request, smb.StatusObjectNameNotFound)
	c.create(t, streamRequest(request.Name, fileOpen), smb.StatusObjectNameNotFound)
	opened := c.create(t, streamRequest(request.Name, fileOpenIf), smb.StatusSuccess)
	if opened.Action != 1 || opened.Size != 0 {
		t.Fatalf("empty stream was changed: %+v", opened)
	}
	c.close(t, opened.ID)
	c.close(t, base)
}
