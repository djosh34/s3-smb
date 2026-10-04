package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The second authenticated login never opens a member of the first login's
// lease. Capture RH before acknowledging RH, independently of Samba's historical
// H-target/RH-ack mismatch. CREATE and ACK both use protected raw traffic and the
// file-backed JuiceFS/SQLite adapter.
func TestLeaseBreakAckOnSecondLoginWithoutMember(t *testing.T) {
	server, holder, _ := newCreateLeaseClients(t)
	other := loginCreateLeaseClient(t, server, 2)
	if other.session.SessionID == holder.session.SessionID || other.session.ClientGUID != holder.session.ClientGUID {
		t.Fatal("second login did not get a distinct same-client session")
	}
	opened := holder.create(t, leaseCreateRequest("cross-login-ack"), leaseV2(4, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite))
	assertLeaseGrant(t, opened, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	binding := state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID}
	open, status := server.options.State.Find(state.FileID(opened.Reply.ID), binding)
	if status != smb.StatusSuccess {
		t.Fatalf("find holder = %#x", status)
	}
	done := startServerBreak(holder.ctx, server, open, smb.LeaseRead|smb.LeaseHandle)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.CurrentState != 7 || notification.NewState != 3 || notification.Flags != 1 || notification.Key != [16]byte(open.LeaseKey) {
		t.Fatalf("valid RH target not captured: %+v", notification)
	}
	other.ack(t, notification)
	finishServerBreak(holder.ctx, t, done)

	// A smaller same-key reopen observes the held RH state and captured epoch;
	// successful acknowledgment neither changes the shared key nor its epoch.
	reopened := holder.create(t, leaseCreateRequest("cross-login-ack"), leaseV2(4, smb.LeaseRead))
	assertLeaseGrant(t, reopened, smb.LeaseRead|smb.LeaseHandle)
	if reopened.Lease.Epoch != notification.Epoch || reopened.Lease.Flags&leaseBreakInProgress != 0 {
		t.Fatalf("acknowledged shared lease = %+v", reopened.Lease)
	}
	response := acknowledgeBreak(other.ctx, t, other.client, other.session, other.next, open.LeaseKey, smb.LeaseRead|smb.LeaseHandle)
	other.next++
	if response.Header.Status != smb.StatusUnsuccessful || response.Header.Command != wire.OplockBreak {
		t.Fatalf("no pending break ACK = %+v", response.Header)
	}
}
