package state

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestReadRevocationWaitsForCapturedDelivery(t *testing.T) {
	for _, rights := range []uint32{smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle} {
		t.Run(map[uint32]string{1: "R no ACK", 3: "RH H ACK withheld"}[rights], func(t *testing.T) {
			checkReadRevocationDelivery(t, rights)
		})
	}
}

func TestQueuedReadRevocationRequiresFinalDroppingReceipt(t *testing.T) {
	table := readDeliveryTable(t)
	open := readDeliveryOpen(t, table, 7)
	first, _ := table.BreakLeases(open.Object, GUID{9}, GUID{9}, 3)
	if len(first) != 1 {
		t.Fatal("initial break missing")
	}
	table.NoteLeaseBreakDelivered(first[0])
	table.BreakLeases(open.Object, GUID{9}, GUID{9}, 0)
	second, actions, status := table.AckBreak(open.Binding, open.ClientGUID, open.LeaseKey, 3)
	if status != smb.StatusSuccess || len(actions) != 0 || len(second) != 1 || second[0].CurrentState != 3 || second[0].NewState != 1 || second[0].Epoch != first[0].Epoch {
		t.Fatalf("queued RH-to-R: %+v, status %#x", second, status)
	}
	table.NoteLeaseBreakDelivered(second[0])
	if readDeliverySnapshot(t, table, open).readRevocationComplete() {
		t.Fatal("held W clearance or RH-to-R delivery proved READ revocation")
	}
	last, actions, status := table.AckBreak(open.Binding, open.ClientGUID, open.LeaseKey, 1)
	if status != smb.StatusSuccess || len(actions) != 0 || len(last) != 1 || last[0].CurrentState != 1 || last[0].NewState != 0 || last[0].AckRequired || last[0].Epoch != first[0].Epoch {
		t.Fatalf("final R-to-NONE: %+v, status %#x", last, status)
	}
	if readDeliverySnapshot(t, table, open).readRevocationComplete() {
		t.Fatal("captured R-to-NONE or a queued wake proved emission")
	}
	table.NoteLeaseBreakDelivered(last[0])
	if !readDeliverySnapshot(t, table, open).readRevocationComplete() {
		t.Fatal("final actual READ-dropping receipt did not prove revocation")
	}
}

func TestReadRevocationExpiryEndsReceiptLifetime(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	table, err := New(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	open := readDeliveryOpen(t, table, 3)
	notification := readDroppingBreak(t, table, open)
	now = now.Add(LeaseBreakTimeout)
	table.ExpireBreaks()
	if !readDeliverySnapshot(t, table, open).readRevocationComplete() {
		t.Fatal("expired lease kept a READ delivery obligation")
	}
	changed := table.BreakChanges()
	table.NoteLeaseBreakDelivered(notification)
	select {
	case <-changed:
		t.Fatal("expired receipt changed current readiness state")
	default:
	}
}

func TestReadDeliveryReceiptRejectsIdenticalWireNewLifetime(t *testing.T) {
	table := readDeliveryTable(t)
	first := readDeliveryOpen(t, table, 1)
	old := readDroppingBreak(t, table, first)
	if _, status := table.Close(first.ID, first.Binding); status != smb.StatusSuccess {
		t.Fatalf("close: %#x", status)
	}
	fresh := readDeliveryOpen(t, table, 1)
	current := readDroppingBreak(t, table, fresh)
	if old.ClientGUID != current.ClientGUID || old.LeaseKey != current.LeaseKey || old.Epoch != current.Epoch || old.CurrentState != current.CurrentState || old.NewState != current.NewState {
		t.Fatal("test did not reproduce identical wire identities across lifetimes")
	}
	table.NoteLeaseBreakDelivered(old)
	if readDeliverySnapshot(t, table, fresh).readRevocationComplete() {
		t.Fatal("stale receipt proved delivery for a recreated lease")
	}
	table.NoteLeaseBreakDelivered(current)
	if !readDeliverySnapshot(t, table, fresh).readRevocationComplete() {
		t.Fatal("current captured receipt did not prove emission")
	}
}

func TestReadDeliveryReceiptRejectsChangedCapturedFields(t *testing.T) {
	table := readDeliveryTable(t)
	open := readDeliveryOpen(t, table, 1)
	notification := readDroppingBreak(t, table, open)
	for _, wrong := range []Break{{ClientGUID: notification.ClientGUID, LeaseKey: notification.LeaseKey, Epoch: notification.Epoch}, notification} {
		wrong.Epoch++
		table.NoteLeaseBreakDelivered(wrong)
		if readDeliverySnapshot(t, table, open).readRevocationComplete() {
			t.Fatal("an uncaptured or changed receipt proved emission")
		}
	}
}
