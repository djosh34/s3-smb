package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateRefusesFileIDAndKeepsConnection(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	request := createRequest("by-id", fileCreateDisposition)
	request.Options = fileOpenByFileID
	requireIOStatus(t, client.create(t, request), smb.StatusNotSupported)
	resolved, err := client.server.options.Storage.Lookup(t.Context(), "by-id")
	if err != nil || resolved.Exists {
		t.Fatalf("refused CREATE mutated namespace: %+v, %v", resolved, err)
	}
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, client.exchange(t, wire.Echo, body, 1), smb.StatusSuccess)
}

func TestCreateDirectoryOptions(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	file := createdFile(t, client.create(t, createRequest("file", fileCreateDisposition)))
	requireIOStatus(t, client.close(t, file.ID, 0), smb.StatusSuccess)
	directoryRequest := createRequest("dir", fileCreateDisposition)
	directoryRequest.Options = fileDirectoryFile
	directory := createdFile(t, client.create(t, directoryRequest))
	if directory.Attributes&0x10 == 0 {
		t.Fatalf("directory CREATE = %+v", directory)
	}
	requireIOStatus(t, client.close(t, directory.ID, 0), smb.StatusSuccess)
	for _, test := range []struct {
		name                 string
		options, disposition uint32
		status               smb.Status
	}{
		{"file", fileDirectoryFile, fileOpen, smb.StatusNotADirectory},
		{"dir", fileNonDirectoryFile, fileOpen, smb.StatusFileIsADirectory},
		{"file", fileDirectoryFile | fileNonDirectoryFile, fileOpen, smb.StatusInvalidParameter},
		{"dir", fileDirectoryFile, fileSupersede, smb.StatusInvalidParameter},
		{"dir", fileDirectoryFile, fileOverwrite, smb.StatusInvalidParameter},
		{"dir", fileDirectoryFile, fileOverwriteIf, smb.StatusInvalidParameter},
	} {
		t.Run(fmt.Sprintf("%s_options_%x_disposition_%d", test.name, test.options, test.disposition), func(t *testing.T) {
			request := createRequest(test.name, test.disposition)
			request.Options = test.options
			requireIOStatus(t, client.create(t, request), test.status)
		})
	}
}

func TestCreateDeclinesDurableWithoutLease(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	context, err := wire.EncodeDurableRequest(wire.DurableRequest{CreateGUID: [16]byte{5}})
	if err != nil {
		t.Fatal(err)
	}
	request := createRequest("DH2Q", fileCreateDisposition)
	request.OplockLevel = 0xff
	request.Contexts = []wire.CreateContext{context}
	response := createdFile(t, client.create(t, request))
	if response.OplockLevel != 0 || len(response.Contexts) != 0 {
		t.Fatalf("unrequested grant: %+v", response)
	}
	open, status := client.server.options.State.Find(state.FileID(response.ID), state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID})
	if status != smb.StatusSuccess || open.Durable || open.LeaseKey != (state.GUID{}) {
		t.Fatalf("CREATE context grant = %+v, status %#x", open, status)
	}
	requireIOStatus(t, client.close(t, response.ID, 0), smb.StatusSuccess)
}

func TestCreateDeleteOnCloseRecordedAndApplied(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	request := createRequest("temporary", fileCreateDisposition)
	request.Options = fileDeleteOnClose
	response := createdFile(t, client.create(t, request))
	open, status := client.server.options.State.Find(state.FileID(response.ID), state.Binding{SessionID: client.session.SessionID, TreeID: client.session.TreeID})
	if status != smb.StatusSuccess || !open.DeleteOnClose {
		t.Fatalf("delete intent = %+v, status %#x", open, status)
	}
	writeCreatedFile(t, client, response.ID, "temporary bytes")
	requireIOStatus(t, client.close(t, response.ID, 1), smb.StatusSuccess)
	resolved, err := client.server.options.Storage.Lookup(t.Context(), "temporary")
	if err != nil || resolved.Exists {
		t.Fatalf("delete-on-close = %+v, %v", resolved, err)
	}
}

func TestCloseSelectedObjectAttributes(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	base := createdFile(t, client.create(t, createRequest("base", fileCreateDisposition)))
	writeCreatedFile(t, client, base.ID, "base payload")
	stream := createdFile(t, client.create(t, createRequest("base:AFP_AfpInfo", fileCreateDisposition)))
	writeCreatedFile(t, client, stream.ID, "info")
	message := client.close(t, stream.ID, 1)
	requireIOStatus(t, message, smb.StatusSuccess)
	response, err := wire.DecodeCloseResponse(message)
	if err != nil || response.Size != 4 || response.Flags != 1 {
		t.Fatalf("stream CLOSE = %+v, %v", response, err)
	}
	readCreatedFile(t, client, base.ID, "base payload")
	message = client.close(t, base.ID, 0)
	requireIOStatus(t, message, smb.StatusSuccess)
	response, err = wire.DecodeCloseResponse(message)
	if err != nil || response != (wire.CloseResponse{}) {
		t.Fatalf("CLOSE without postquery = %+v, %v", response, err)
	}
}

func TestCreateInvalidParametersLeaveNamespace(t *testing.T) {
	client := newReadWriteClient(t, newFilesMetaStorage(t))
	for _, request := range []wire.CreateRequest{
		{Name: "bad", DesiredAccess: genericAll, Disposition: 6, ShareAccess: 7},
		{Name: "bad", DesiredAccess: genericAll, Disposition: fileCreateDisposition, ShareAccess: 8},
	} {
		requireIOStatus(t, client.create(t, request), smb.StatusInvalidParameter)
	}
	resolved, err := client.server.options.Storage.Lookup(t.Context(), "bad")
	if err != nil || resolved.Exists {
		t.Fatalf("invalid CREATE mutated namespace: %+v, %v", resolved, err)
	}
}
