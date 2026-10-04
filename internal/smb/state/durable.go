package state

import (
	"slices"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func (table *Table) openIDs() []uint64 {
	ids := make([]uint64, 0, len(table.opens))
	for id := range table.opens {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (table *Table) abortMatching(match func(Binding) bool) {
	for token, request := range table.reservations {
		if match(request.Binding) {
			table.releaseReservation(token, request)
			table.prune(request.Object)
		}
	}
}

func (table *Table) closeMatching(match func(Open) bool) []CloseAction {
	var actions []CloseAction
	for _, id := range table.openIDs() {
		open := table.opens[id]
		if match(open.Open) {
			actions = append(actions, table.closeOpen(open))
		}
	}
	return actions
}

// Disconnect detaches durable opens without storage calls and closes the rest.
// Outstanding CREATE reservations on the old session cannot publish grants.
func (table *Table) Disconnect(sessionID uint64) []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	if sessionID == 0 {
		return nil
	}
	table.abortMatching(func(binding Binding) bool { return binding.SessionID == sessionID })
	now := table.now()
	for _, open := range table.opens {
		if open.Binding.SessionID == sessionID && open.Durable {
			open.Binding = Binding{}
			open.DurableDeadline = now.Add(open.DurableTimeout)
			table.signalBreakChanges()
		}
	}
	actions := table.closeMatching(func(open Open) bool { return open.Binding.SessionID == sessionID })
	return append(actions, table.closeDetachedBreaks()...)
}

// CloseSession closes attached session opens, including durable opens, on LOGOFF.
func (table *Table) CloseSession(sessionID uint64) []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	if sessionID == 0 {
		return nil
	}
	table.abortMatching(func(binding Binding) bool { return binding.SessionID == sessionID })
	return table.closeMatching(func(open Open) bool { return open.Binding.SessionID == sessionID })
}

// CloseTree closes attached tree opens, including durable opens.
func (table *Table) CloseTree(binding Binding) []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	if !validBinding(binding) {
		return nil
	}
	table.abortMatching(func(candidate Binding) bool { return candidate == binding })
	return table.closeMatching(func(open Open) bool { return open.Binding == binding })
}

func (table *Table) reconnectCandidate(request ReconnectRequest) (*openEntry, smb.Status) {
	open := table.opens[request.ID.Persistent]
	if !validBinding(request.Binding) {
		return nil, smb.StatusInvalidParameter
	}
	if open == nil || open.ID != request.ID || !open.Durable || open.Binding.SessionID != 0 || !open.DurableDeadline.After(table.now()) ||
		open.Share != request.Share || open.ClientGUID != request.ClientGUID ||
		open.CreateGUID != request.CreateGUID || open.LeaseKey != request.LeaseKey {
		return nil, smb.StatusObjectNameNotFound
	}
	if open.User != request.User {
		return nil, smb.StatusAccessDenied
	}
	return open, smb.StatusSuccess
}

// ReconnectCandidate validates durable identities and the detached deadline
// without changing the open. A different durable owner receives ACCESS_DENIED.
// The server may check the namespace before Reconnect repeats these checks.
func (table *Table) ReconnectCandidate(request ReconnectRequest) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.reconnectCandidate(request)
	if status != smb.StatusSuccess {
		return Open{}, status
	}
	return open.Open, smb.StatusSuccess
}

// Reconnect validates all durable identities and installs a fresh volatile ID.
func (table *Table) Reconnect(request ReconnectRequest) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.reconnectCandidate(request)
	if status != smb.StatusSuccess {
		return Open{}, status
	}
	if !availableID(table.nextVolatile) {
		return Open{}, smb.StatusInsufficientResources
	}
	table.nextVolatile++
	open.ID.Volatile = table.nextVolatile
	open.Binding = request.Binding
	open.DurableDeadline = time.Time{}
	table.signalBreakChanges()
	return open.Open, smb.StatusSuccess
}

// Expire closes detached opens at their granted deadline, returning cleanup.
func (table *Table) Expire() []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	now := table.now()
	return table.closeMatching(func(open Open) bool {
		return open.Binding.SessionID == 0 && !open.DurableDeadline.After(now)
	})
}

// CloseAll closes attached and detached opens and aborts all reservations.
func (table *Table) CloseAll() []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.abortMatching(func(Binding) bool { return true })
	return table.closeMatching(func(Open) bool { return true })
}
