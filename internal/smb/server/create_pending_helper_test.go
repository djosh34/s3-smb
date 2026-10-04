package server

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFileCreateHelperWaitsForFinalReply(t *testing.T) {
	pendingBody, err := wire.EncodeErrorResponse(wire.ErrorResponse{})
	if err != nil {
		t.Fatal(err)
	}
	fileID := wire.FileID{Persistent: 4, Volatile: 5}
	finalBody, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: fileID, Action: 2})
	if err != nil {
		t.Fatal(err)
	}
	header := wire.Header{Command: wire.Create, MessageID: 9, SessionID: 77, AsyncID: 12, Flags: wire.FlagResponse | wire.FlagAsync, Status: smb.StatusPending, CreditCharge: 1, Credit: 1}
	pendingFrame := streamFrame(t, wire.Message{Header: header, Body: pendingBody})
	header.Status, header.Credit = smb.StatusSuccess, 0
	finalFrame := streamFrame(t, wire.Message{Header: header, Body: finalBody})
	conn := streamPeer(t, func(peer net.Conn) error {
		if _, readErr := readFrame(peer, smb.CreditUnit); readErr != nil {
			return readErr
		}
		if _, writeErr := io.Copy(peer, bytes.NewReader(pendingFrame)); writeErr != nil {
			return writeErr
		}
		_, writeErr := io.Copy(peer, bytes.NewReader(finalFrame))
		return writeErr
	})
	client, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	response := createdFile(t, fileCreate(t.Context(), t, client, smbtest.Session{SessionID: 77, TreeID: 12}, 9, createRequest("pending", fileOpenIf)))
	if response.ID != fileID || response.Action != 2 {
		t.Fatalf("final CREATE = %+v", response)
	}
}
