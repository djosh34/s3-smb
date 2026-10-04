package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestAppendWriteRechecksEOFAfterCompetingWrite(t *testing.T) {
	for _, access := range []struct {
		name string
		mask uint32
	}{
		{"ordinary writer", fileReadData | fileWriteData},
		{"append writer", fileReadData | fileAppendData},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", access.name, stream), func(t *testing.T) {
				checkStaleAppendWrite(t, access.mask, stream)
			})
		}
	}
}

func TestAppendSupersedeInitializesWithoutGrantingOverwrite(t *testing.T) {
	client := newReadWriteClient(t, newIOFixture(t, nil).adapter)
	initial := createdFile(t, client.create(t, createRequest("supersede", fileCreateDisposition)))
	writeCreatedFile(t, client, initial.ID, "old data")
	request := createRequest("supersede", fileSupersede)
	request.DesiredAccess = fileDelete | fileReadData | fileAppendData
	appender := createdFile(t, client.create(t, request))
	if appender.Size != 0 || appender.Action != 0 {
		t.Fatalf("supersede: %+v", appender)
	}
	readCreatedFile(t, client, initial.ID, "")
	writeCreatedFile(t, client, appender.ID, "new")
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: appender.ID, Data: []byte("bad")}, 1), smb.StatusAccessDenied)
	requireIOStatus(t, client.write(t, wire.WriteRequest{ID: appender.ID, Offset: 3, Data: []byte(" tail")}, 1), smb.StatusSuccess)
	readCreatedFile(t, client, initial.ID, "new tail")
}
