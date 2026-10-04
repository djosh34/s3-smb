package state

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

type readDeliveryHandle struct{ object smb.ObjectKey }

func (handle readDeliveryHandle) Key() smb.ObjectKey { return handle.object }

func readDeliveryOpen(t *testing.T, table *Table, rights uint32) Open {
	t.Helper()
	request := OpenRequest{Object: smb.ObjectKey{Inode: 51}, Binding: Binding{SessionID: 1, TreeID: 1}, ClientGUID: GUID{3}, GrantedAccess: 1, Sharing: 7}
	reservation, status := table.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatalf("reserve: %#x", status)
	}
	grant := Grant{Handle: readDeliveryHandle{request.Object}, Lease: Lease{ClientGUID: request.ClientGUID, Key: GUID{4}, State: rights, Epoch: 17}}
	open, status := table.Commit(reservation, grant)
	if status != smb.StatusSuccess {
		t.Fatalf("commit: %#x", status)
	}
	return open
}

func readDeliveryTable(t *testing.T) *Table {
	t.Helper()
	table, err := New(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func readDroppingBreak(t *testing.T, table *Table, open Open) Break {
	t.Helper()
	notifications, actions := table.BreakLeases(open.Object, GUID{9}, GUID{9}, 0)
	if len(notifications) != 1 || len(actions) != 0 {
		t.Fatalf("break: %+v, cleanup %+v", notifications, actions)
	}
	return notifications[0]
}

func readDeliverySnapshot(t *testing.T, table *Table, open Open) Lease {
	t.Helper()
	lease, exists := table.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists {
		t.Fatal("lease missing")
	}
	return lease
}

func TestReadRevocationWaitsForCapturedDelivery(t *testing.T) {
	for _, rights := range []uint32{smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle} {
		t.Run(map[uint32]string{1: "R no ACK", 3: "RH H ACK withheld"}[rights], func(t *testing.T) {
			checkReadRevocationDelivery(t, rights)
		})
	}
}

func checkReadRevocationDelivery(t *testing.T, rights uint32) {
	t.Helper()
	table := readDeliveryTable(t)
	open := readDeliveryOpen(t, table, rights)
	notification := readDroppingBreak(t, table, open)
	before := readDeliverySnapshot(t, table, open)
	if before.readRevocationComplete() {
		t.Fatal("capturing or clearing held R proved emission")
	}
	changed := table.BreakChanges()
	table.NoteLeaseBreakDelivered(notification)
	after := readDeliverySnapshot(t, table, open)
	if !after.readRevocationComplete() || after.State != before.State || after.BreakTo != before.BreakTo || after.Epoch != before.Epoch || after.Deadline != before.Deadline || after.Breaking != before.Breaking {
		t.Fatalf("receipt changed protocol state or failed to prove emission: before %+v, after %+v", before, after)
	}
	if before.readRevocationComplete() {
		t.Fatal("a copied snapshot changed after delivery")
	}
	select {
	case <-changed:
	default:
		t.Fatal("successful emission did not wake readiness observers")
	}
	if rights == 3 && !table.BreakPending(notification) {
		t.Fatal("READ emission completed the withheld H acknowledgment")
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
