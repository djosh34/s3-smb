package server

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestClosePostqueryWaitsForHeldWriter(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	created := createdFile(t, client.create(t, createRequest("closing", fileCreateDisposition)))
	binding := state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID}
	open, status := client.server.options.State.Find(state.FileID(created.ID), binding)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	request := openRequestContext(client.server, open)
	writer, release, status := useOpen(request, created.ID)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	// Failure paths must release the writer before the client/server cleanup.
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: created.ID, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.Close, body, 1)
	if sendErr := client.client.Send(client.ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		_, status := request.Opens.Find(open.ID, binding)
		if status == smb.StatusFileClosed {
			break
		}
		if status != smb.StatusSuccess {
			t.Fatal(status)
		}
		select {
		case <-ticker.C:
		case <-client.ctx.Done():
			t.Fatal("CLOSE did not remove the open before draining")
		}
	}
	// A held user can still need the namespace guard while CLOSE drains it.
	unlock, err := lockParent(client.ctx, request, 1)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := request.Storage.WriteAt(client.ctx, writer.Handle, []byte("1234567"), 0)
	unlock()
	if writeErr != nil || n != 7 {
		t.Fatalf("held write: %d, %v", n, writeErr)
	}
	release()
	held = false
	reply, err := client.client.Receive(client.ctx)
	if err != nil || len(reply.Messages) != 1 {
		t.Fatalf("CLOSE reply: %+v, %v", reply, err)
	}
	requireIOStatus(t, reply.Messages[0], smb.StatusSuccess)
	response, err := wire.DecodeCloseResponse(reply.Messages[0])
	if err != nil || response.Size != 7 || response.Flags != 1 {
		t.Fatalf("CLOSE postquery: %+v, %v", response, err)
	}
}

func TestClosePostqueryErrorRetainsRelatedFileID(t *testing.T) {
	storage := &createFaultStorage{Storage: newFilesMetaStorage(t)}
	client := newReadWriteClient(t, storage)
	first := createdFile(t, client.create(t, createRequest("surviving", fileCreateDisposition)))
	writeCreatedFile(t, client, first.ID, "survivor")
	second := createdFile(t, client.create(t, createRequest("closing", fileCreateDisposition)))
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: second.ID, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	messages := []wire.Message{
		compoundFileRequest(t, client.session, wire.Read, client.next, first.ID, false),
		compoundFileRequest(t, client.session, wire.Close, client.next+1, second.ID, true),
		compoundFileRequest(t, client.session, wire.Echo, client.next+2, wire.FileID{}, true),
		compoundFileRequest(t, client.session, wire.Read, client.next+3, placeholderFileID(), true),
	}
	messages[1].Body = body
	client.next += uint64(len(messages))
	storage.failure.Store(createFailGetAttr)
	if sendErr := client.client.Send(client.ctx, messages); sendErr != nil {
		t.Fatal(sendErr)
	}
	statuses := compoundFinalStatuses(client.ctx, t, client.client, len(messages))
	for index, want := range []smb.Status{smb.StatusSuccess, smb.StatusIODeviceError, smb.StatusSuccess, smb.StatusFileClosed} {
		if got := statuses[messages[index].Header.MessageID]; got != want {
			t.Fatalf("member %d status %#x, want %#x", index, got, want)
		}
	}
	storage.failure.Store(0)
	readCreatedFile(t, client, first.ID, "survivor")
	requireIOStatus(t, client.close(t, first.ID, 0), smb.StatusSuccess)
}

func TestClosePostqueryFailureStillDeletesOnce(t *testing.T) {
	storage := &createFaultStorage{Storage: newFilesMetaStorage(t)}
	client := newReadWriteClient(t, storage)
	request := createRequest("delete-after-query-error", fileCreateDisposition)
	request.Options = fileDeleteOnClose
	created := createdFile(t, client.create(t, request))
	storage.failure.Store(createFailGetAttr)
	requireIOStatus(t, client.close(t, created.ID, 1), smb.StatusIODeviceError)
	if storage.opens.Load() != 1 || storage.closes.Load() != 1 {
		t.Fatalf("CLOSE references: opens %d, closes %d", storage.opens.Load(), storage.closes.Load())
	}
	storage.failure.Store(0)
	resolved, err := storage.Lookup(t.Context(), request.Name)
	if err != nil || resolved.Exists {
		t.Fatalf("query error retained delete-on-close entry: %+v, %v", resolved, err)
	}
	requireIOStatus(t, client.close(t, created.ID, 1), smb.StatusFileClosed)
	if storage.closes.Load() != 1 {
		t.Fatal("failed query closed storage twice")
	}
}
