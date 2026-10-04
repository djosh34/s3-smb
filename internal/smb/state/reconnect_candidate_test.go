package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestReconnectCandidateDoesNotAttachOrExtendDeadline(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	table.Disconnect(open.Binding.SessionID)
	reconnect := reconnectRequest(open)
	first, status := table.ReconnectCandidate(reconnect)
	statusIs(t, status, smb.StatusSuccess)
	second, status := table.ReconnectCandidate(reconnect)
	statusIs(t, status, smb.StatusSuccess)
	if first != second || first.ID != open.ID || first.Binding != (state.Binding{}) || first.DurableDeadline != now.Add(smb.DefaultDurableTimeout) {
		t.Fatalf("candidate changed detached open: %+v, %+v", first, second)
	}
	*now = now.Add(smb.DefaultDurableTimeout)
	_, status = table.ReconnectCandidate(reconnect)
	statusIs(t, status, smb.StatusObjectNameNotFound)
	_, status = table.Reconnect(reconnect)
	statusIs(t, status, smb.StatusObjectNameNotFound)
}

func TestReconnectRevalidatesCandidateAndKeepsVolatileSequence(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	table.Disconnect(open.Binding.SessionID)
	reconnect := reconnectRequest(open)
	_, status := table.ReconnectCandidate(reconnect)
	statusIs(t, status, smb.StatusSuccess)
	wrongOwner := reconnect
	wrongOwner.User = "other"
	_, status = table.ReconnectCandidate(wrongOwner)
	statusIs(t, status, smb.StatusAccessDenied)
	_, status = table.Reconnect(wrongOwner)
	statusIs(t, status, smb.StatusAccessDenied)
	*now = now.Add(time.Second)
	reattached, status := table.Reconnect(reconnect)
	statusIs(t, status, smb.StatusSuccess)
	if reattached.ID.Volatile != open.ID.Volatile+1 {
		t.Fatalf("candidate or refusal consumed volatile ID: %+v", reattached.ID)
	}
	_, status = table.ReconnectCandidate(reconnect)
	statusIs(t, status, smb.StatusObjectNameNotFound)
}
