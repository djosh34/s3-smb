package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func (client *readWriteClient) create(t *testing.T, request wire.CreateRequest) wire.Message {
	t.Helper()
	message := fileCreate(client.ctx, t, client.client, client.session, client.next, request)
	client.next++
	return message
}

func (client *readWriteClient) close(t *testing.T, id wire.FileID, flags uint16) wire.Message {
	t.Helper()
	message := fileClose(client.ctx, t, client.client, client.session, client.next, id, flags)
	client.next++
	return message
}

func createRequest(name string, disposition uint32) wire.CreateRequest {
	return wire.CreateRequest{Name: name, DesiredAccess: 0x10000000, ShareAccess: 7, Disposition: disposition}
}

func readCreatedFile(t *testing.T, client *readWriteClient, id wire.FileID, want string) {
	t.Helper()
	response := client.read(t, wire.ReadRequest{ID: id, Length: 64}, 1)
	if want == "" {
		requireIOStatus(t, response, smb.StatusEndOfFile)
		return
	}
	requireIOStatus(t, response, smb.StatusSuccess)
	read, err := wire.DecodeReadResponse(response)
	if err != nil || string(read.Data) != want {
		t.Fatalf("READ = %q, %v; want %q", read.Data, err, want)
	}
}

func writeCreatedFile(t *testing.T, client *readWriteClient, id wire.FileID, data string) {
	t.Helper()
	response := client.write(t, wire.WriteRequest{ID: id, Data: []byte(data)}, 1)
	requireIOStatus(t, response, smb.StatusSuccess)
	write, err := wire.DecodeWriteResponse(response)
	if err != nil || uint64(write.Count) != uint64(len(data)) {
		t.Fatalf("WRITE = %+v, %v", write, err)
	}
}

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

func checkCreateDisposition(t *testing.T, client *readWriteClient, disposition uint32, exists bool, replacement string) {
	t.Helper()
	var first wire.FileID
	const old = "old payload"
	if exists {
		created := createdFile(t, client.create(t, createRequest("file", fileCreateDisposition)))
		first = created.ID
		writeCreatedFile(t, client, first, old)
	}
	response := client.create(t, createRequest("file", disposition))
	if exists && disposition == fileCreateDisposition {
		requireIOStatus(t, response, smb.StatusObjectNameCollision)
		readCreatedFile(t, client, first, old)
		requireIOStatus(t, client.close(t, first, 0), smb.StatusSuccess)
		return
	}
	if !exists && (disposition == fileOpen || disposition == fileOverwrite) {
		requireIOStatus(t, response, smb.StatusObjectNameNotFound)
		return
	}
	created := createdFile(t, response)
	wantAction := uint32(2)
	before := ""
	if exists {
		switch disposition {
		case fileOpen, fileOpenIf:
			wantAction, before = 1, old
		case fileSupersede:
			wantAction = 0
		case fileOverwrite, fileOverwriteIf:
			wantAction = 3
		}
	}
	if created.Action != wantAction || created.Size != uint64(len(before)) {
		t.Fatalf("CREATE = %+v, want action %d, size %d", created, wantAction, len(before))
	}
	readCreatedFile(t, client, created.ID, before)
	if exists {
		readCreatedFile(t, client, first, before)
	}
	writeCreatedFile(t, client, created.ID, replacement)
	want := replacement
	if len(before) > len(replacement) {
		want += before[len(replacement):]
	}
	requireIOStatus(t, client.close(t, created.ID, 0), smb.StatusSuccess)
	if exists {
		readCreatedFile(t, client, first, want)
		requireIOStatus(t, client.close(t, first, 0), smb.StatusSuccess)
	}
	reopened := createdFile(t, client.create(t, createRequest("file", fileOpen)))
	readCreatedFile(t, client, reopened.ID, want)
	requireIOStatus(t, client.close(t, reopened.ID, 0), smb.StatusSuccess)
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
