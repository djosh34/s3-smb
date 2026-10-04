package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestAckBreakFromSameClientDifferentSession(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	open := commit(t, table, req, grant)
	notification := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	if notification.CurrentState != 7 || notification.NewState != 3 || !notification.AckRequired {
		t.Fatalf("captured RH break = %+v", notification)
	}
	// The caller validates this other session before crossing the table seam;
	// it has no member open and the table must not impose such a condition.
	_, actions, status := table.AckBreak(state.Binding{SessionID: 2}, req.ClientGUID, open.LeaseKey, smb.LeaseRead|smb.LeaseHandle)
	statusIs(t, status, smb.StatusSuccess)
	if len(actions) != 0 {
		t.Fatalf("RH ACK released storage: %+v", actions)
	}
	current, exists := table.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || current.State != 3 || current.Breaking || current.Epoch != notification.Epoch {
		t.Fatalf("acknowledged shared lease = %+v", current)
	}
	_, _, status = table.AckBreak(state.Binding{SessionID: 2}, req.ClientGUID, open.LeaseKey, smb.LeaseRead|smb.LeaseHandle)
	statusIs(t, status, smb.StatusUnsuccessful)
}
