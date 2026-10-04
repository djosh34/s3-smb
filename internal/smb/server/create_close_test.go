package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateCloseRealAdapter(t *testing.T) {
	server, client, ctx, session := newFileClient(t)
	id := session.NextMessageID
	create := wire.CreateRequest{Name: "backup", DesiredAccess: 0x10000000, ShareAccess: 7, Disposition: fileCreateDisposition}
	response := createdFile(t, fileCreate(ctx, t, client, session, id, create))
	if response.Action != 2 || response.OplockLevel != 0 || response.Size != 0 {
		t.Fatalf("CREATE = %+v", response)
	}
	closeMessage := fileClose(ctx, t, client, session, id+1, response.ID, 1)
	if closeMessage.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE = %#x", closeMessage.Header.Status)
	}
	closeReply, err := wire.DecodeCloseResponse(closeMessage)
	if err != nil {
		t.Fatal(err)
	}
	if closeReply.Flags != 1 || closeReply.Size != 0 {
		t.Fatalf("CLOSE = %+v", closeReply)
	}
	if _, status := server.options.State.Find(state.FileID(response.ID), state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}); status != smb.StatusFileClosed {
		t.Fatalf("closed grant lookup = %#x", status)
	}
	if got := fileClose(ctx, t, client, session, id+2, response.ID, 0).Header.Status; got != smb.StatusFileClosed {
		t.Fatalf("second CLOSE = %#x", got)
	}
}
