package state

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// From lease_read_delivery_test.go.
type readDeliveryHandle struct{ object smb.ObjectKey }

// From lease_read_delivery_test.go.
func (handle readDeliveryHandle) Key() smb.ObjectKey { return handle.object }

// From lease_read_delivery_test.go.
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

// From lease_read_delivery_test.go.
func readDeliveryTable(t *testing.T) *Table {
	t.Helper()
	table, err := New(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// From lease_read_delivery_test.go.
func readDroppingBreak(t *testing.T, table *Table, open Open) Break {
	t.Helper()
	notifications, actions := table.BreakLeases(open.Object, GUID{9}, GUID{9}, 0)
	if len(notifications) != 1 || len(actions) != 0 {
		t.Fatalf("break: %+v, cleanup %+v", notifications, actions)
	}
	return notifications[0]
}

// From lease_read_delivery_test.go.
func readDeliverySnapshot(t *testing.T, table *Table, open Open) Lease {
	t.Helper()
	lease, exists := table.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists {
		t.Fatal("lease missing")
	}
	return lease
}

// From lease_read_delivery_test.go.
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

// From mutation_gate_test.go.
type mutationHandle struct{ object smb.ObjectKey }

// From mutation_gate_test.go.
func (handle mutationHandle) Key() smb.ObjectKey { return handle.object }
