package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateSharingViolationReaderBreakKeepsReadCaching(t *testing.T) {
	_, first, second := newCreateLeaseClients(t)
	held := leaseCreateRequest("file")
	held.ShareAccess = 0
	first.create(t, held, leaseV2(1, 3))
	id := second.send(t, leaseCreateRequest("file"), nil)
	notification, err := first.client.WaitLeaseBreak(first.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.CurrentState != 3 || notification.NewState != 1 || notification.Flags != 1 {
		t.Fatalf("reader sharing break = %+v", notification)
	}
	done := receiveCreateLater(second, id)
	assertCreateWaits(t, done)
	first.ack(t, notification)
	result := <-done
	if result.err != nil || result.message.Header.Status != smb.StatusSharingViolation {
		t.Fatalf("second sharing check = %#x, error %v", result.message.Header.Status, result.err)
	}
}
