package state

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

const (
	allRights           = RightRead | RightWrite | RightDelete
	deleteAccess uint32 = 0x00010000
)

// New creates an empty table with an injected clock.
func New(now func() time.Time) (*Table, error) {
	if now == nil {
		return nil, errors.New("state: clock is required")
	}
	return &Table{
		now:          now,
		opens:        make(map[uint64]*openEntry),
		reservations: make(map[Reservation]OpenRequest),
		objects:      make(map[smb.ObjectKey]*objectEntry),
		creates:      make(map[createIdentity]createEntry),
		leaseObjects: make(map[leaseIdentity]smb.ObjectKey),
	}, nil
}

func identity(request OpenRequest) createIdentity {
	return createIdentity{user: request.User, share: request.Share, client: request.ClientGUID, create: request.CreateGUID}
}

func openRequest(open Open) OpenRequest {
	return OpenRequest{
		User: open.User, Share: open.Share, Object: open.Object, Binding: open.Binding,
		ClientGUID: open.ClientGUID, CreateGUID: open.CreateGUID, CreateParameters: open.CreateParameters,
		GrantedAccess: open.GrantedAccess, SharingIntent: open.SharingIntent, Sharing: open.Sharing,
	}
}

func validBinding(binding Binding) bool {
	return binding.SessionID != 0 && binding.TreeID != 0
}

func availableID(id uint64) bool {
	return id < math.MaxUint64-1
}

// Reserve checks sharing and deletion before acquiring a CREATE reservation.
func (table *Table) Reserve(request OpenRequest) (Reservation, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if request.Object.Inode == 0 || !validBinding(request.Binding) || request.SharingIntent & ^allRights != 0 || Rights(request.Sharing) & ^allRights != 0 {
		return 0, smb.StatusInvalidParameter
	}
	if request.CreateGUID != (GUID{}) {
		if _, exists := table.creates[identity(request)]; exists {
			return 0, smb.StatusDuplicateObjectID
		}
	}
	if table.deletePending(request.Object) {
		return 0, smb.StatusDeletePending
	}
	if !table.sharingAllowed(request, 0, 0) {
		return 0, smb.StatusSharingViolation
	}
	if !availableID(table.nextReservation) {
		return 0, smb.StatusInsufficientResources
	}
	table.nextReservation++
	reservation := Reservation(table.nextReservation)
	table.reservations[reservation] = request
	table.object(request.Object)
	if request.CreateGUID != (GUID{}) {
		table.creates[identity(request)] = createEntry{reservation: reservation}
	}
	return reservation, smb.StatusSuccess
}

func sharingCompatible(left, right OpenRequest) bool {
	if left.Object.Inode != right.Object.Inode {
		return true
	}
	if left.Object.Stream == right.Object.Stream {
		return left.SharingIntent & ^Rights(right.Sharing) == 0 && right.SharingIntent & ^Rights(left.Sharing) == 0
	}
	// Base deletion checks every stream's deny-delete share, in both orders.
	if left.Object.Stream == "" && left.SharingIntent&RightDelete != 0 && Rights(right.Sharing)&RightDelete == 0 {
		return false
	}
	return right.Object.Stream != "" || right.SharingIntent&RightDelete == 0 || Rights(left.Sharing)&RightDelete != 0
}

func (table *Table) sharingAllowed(request OpenRequest, except uint64, reservation Reservation) bool {
	for id, open := range table.opens {
		if id != except && !sharingCompatible(request, openRequest(open.Open)) {
			return false
		}
	}
	for token, reserved := range table.reservations {
		if token != reservation && !sharingCompatible(request, reserved) {
			return false
		}
	}
	return true
}

func (table *Table) deletePending(key smb.ObjectKey) bool {
	if record := table.objects[key]; record != nil && record.DeletePending {
		return true
	}
	base := table.objects[smb.ObjectKey{Inode: key.Inode}]
	return base != nil && base.DeletePending
}

func (table *Table) object(key smb.ObjectKey) *objectEntry {
	record := table.objects[key]
	if record == nil {
		record = &objectEntry{ObjectRecord: ObjectRecord{Key: key}}
		table.objects[key] = record
	}
	return record
}

// Abort releases a reservation. A consumed or unknown token is invalid.
func (table *Table) Abort(reservation Reservation) smb.Status {
	table.mu.Lock()
	defer table.mu.Unlock()
	request, exists := table.reservations[reservation]
	if !exists {
		return smb.StatusInvalidParameter
	}
	table.releaseReservation(reservation, request)
	table.prune(request.Object)
	return smb.StatusSuccess
}

func (table *Table) releaseReservation(reservation Reservation, request OpenRequest) {
	delete(table.reservations, reservation)
	if request.CreateGUID != (GUID{}) {
		delete(table.creates, identity(request))
	}
}

// Commit replaces a reservation with an open. Invalid grants leave it reserved.
func (table *Table) Commit(reservation Reservation, grant Grant) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	request, exists := table.reservations[reservation]
	if !exists {
		return Open{}, smb.StatusInvalidParameter
	}
	if status := table.validateGrant(request, reservation, grant); status != smb.StatusSuccess {
		return Open{}, status
	}
	if !availableID(table.nextPersistent) || !availableID(table.nextVolatile) {
		return Open{}, smb.StatusInsufficientResources
	}
	table.nextPersistent++
	table.nextVolatile++
	open := Open{
		Handle: grant.Handle, User: request.User, Share: request.Share, Object: request.Object,
		ID: FileID{Persistent: table.nextPersistent, Volatile: table.nextVolatile}, Binding: request.Binding,
		ClientGUID: request.ClientGUID, CreateGUID: request.CreateGUID, CreateParameters: request.CreateParameters,
		GrantedAccess: request.GrantedAccess, SharingIntent: request.SharingIntent, Sharing: request.Sharing,
		DeleteOnClose: grant.DeleteOnClose, Durable: grant.DurableTimeout > 0, DurableTimeout: grant.DurableTimeout,
	}
	if grant.Lease.State != 0 {
		open.LeaseKey = grant.Lease.Key
		table.commitLease(request.Object, grant.Lease)
	}
	table.releaseReservation(reservation, request)
	table.opens[open.ID.Persistent] = &openEntry{Open: open, deleteName: grant.DeleteName}
	record := table.object(request.Object)
	record.Opens = append(record.Opens, open.ID.Persistent)
	if grant.DeleteOnClose {
		if !record.DeletePending {
			record.DeleteName = grant.DeleteName
		}
		record.DeletePending = true
	}
	if request.CreateGUID != (GUID{}) {
		table.creates[identity(request)] = createEntry{persistent: open.ID.Persistent}
	}
	return open, smb.StatusSuccess
}

func validName(name smb.Name, object smb.ObjectKey) bool {
	return name.Parent != 0 && name.Base != "" && name.Stream == object.Stream
}

func (table *Table) validateGrant(request OpenRequest, reservation Reservation, grant Grant) smb.Status {
	if grant.Handle == nil || grant.DurableTimeout < 0 || grant.DurableTimeout > smb.MaxDurableTimeout {
		return smb.StatusInvalidParameter
	}
	if grant.DeleteOnClose {
		if request.GrantedAccess&deleteAccess == 0 {
			return smb.StatusAccessDenied
		}
		if !validName(grant.DeleteName, request.Object) {
			return smb.StatusInvalidParameter
		}
		deleting := request
		deleting.SharingIntent |= RightDelete
		if !table.sharingAllowed(deleting, 0, reservation) {
			return smb.StatusSharingViolation
		}
	}
	if status := table.validateLease(request, reservation, grant); status != smb.StatusSuccess {
		return status
	}
	if grant.DurableTimeout != 0 && (grant.Lease.State&smb.LeaseHandle == 0 || request.CreateGUID == (GUID{})) {
		return smb.StatusInvalidParameter
	}
	return smb.StatusSuccess
}

// Replay finds a marked duplicate CREATE without changing the original open.
func (table *Table) Replay(request OpenRequest) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	entry, exists := table.creates[identity(request)]
	if !exists || entry.persistent == 0 {
		return Open{}, smb.StatusObjectNameNotFound
	}
	open := table.opens[entry.persistent]
	if openRequest(open.Open) != request || !validBinding(request.Binding) {
		return Open{}, smb.StatusInvalidParameter
	}
	return open.Open, smb.StatusSuccess
}

func (table *Table) find(id FileID, binding Binding) (*openEntry, smb.Status) {
	open := table.opens[id.Persistent]
	if open == nil || open.ID != id || open.Binding != binding || !validBinding(binding) {
		return nil, smb.StatusFileClosed
	}
	return open, smb.StatusSuccess
}

// Find validates the full ID and current binding and returns a snapshot.
func (table *Table) Find(id FileID, binding Binding) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return Open{}, status
	}
	return open.Open, smb.StatusSuccess
}

// SetDirectory saves a cursor, reusing the search pattern on continuation.
func (table *Table) SetDirectory(id FileID, binding Binding, cursor DirectoryCursor) smb.Status {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return status
	}
	if cursor.Pattern == "" {
		cursor.Pattern = open.Directory.Pattern
	}
	open.Directory = cursor
	return smb.StatusSuccess
}

// SetDelete changes this open's deletion intent, not another open's intent.
func (table *Table) SetDelete(id FileID, binding Binding, name smb.Name, pending bool) smb.Status {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return status
	}
	if open.GrantedAccess&deleteAccess == 0 {
		return smb.StatusAccessDenied
	}
	if pending {
		if !validName(name, open.Object) {
			return smb.StatusInvalidParameter
		}
		request := openRequest(open.Open)
		request.SharingIntent |= RightDelete
		if !table.sharingAllowed(request, id.Persistent, 0) {
			return smb.StatusSharingViolation
		}
		open.deleteName = name
	}
	record := table.objects[open.Object]
	if pending && !record.DeletePending {
		record.DeleteName = name
	}
	open.DeleteOnClose = pending
	table.refreshDelete(record)
	return smb.StatusSuccess
}

func (table *Table) refreshDelete(record *objectEntry) {
	record.DeletePending = record.deleteCommitted
	for _, id := range record.Opens {
		if table.opens[id].DeleteOnClose {
			record.DeletePending = true
		}
	}
	if !record.DeletePending {
		record.DeleteName = smb.Name{}
	}
}

func (table *Table) prune(key smb.ObjectKey) {
	record := table.objects[key]
	if record == nil || len(record.Opens) != 0 || len(record.Locks) != 0 || len(record.Leases) != 0 || record.DeletePending {
		return
	}
	for _, reserved := range table.reservations {
		if reserved.Object == key {
			return
		}
	}
	delete(table.objects, key)
}

// Close removes an attached open and transfers storage cleanup to the caller.
func (table *Table) Close(id FileID, binding Binding) (CloseAction, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return CloseAction{}, status
	}
	return table.closeOpen(open), smb.StatusSuccess
}

func (table *Table) closeOpen(open *openEntry) CloseAction {
	key := open.Object
	record := table.objects[key]
	action := CloseAction{Handle: open.Handle, Object: key}
	if open.DeleteOnClose {
		record.deleteCommitted = true
		if !record.DeletePending {
			record.DeleteName = open.deleteName
		}
	}
	delete(table.opens, open.ID.Persistent)
	if open.CreateGUID != (GUID{}) {
		delete(table.creates, identity(openRequest(open.Open)))
	}
	record.Opens = slices.DeleteFunc(record.Opens, func(id uint64) bool { return id == open.ID.Persistent })
	record.Locks = slices.DeleteFunc(record.Locks, func(lock Range) bool { return lock.Owner == open.ID.Persistent })
	table.releaseLeases(record)
	table.refreshDelete(record)
	// A pending base deletion takes precedence when the inode's last open closes.
	baseKey := smb.ObjectKey{Inode: key.Inode}
	base := table.objects[baseKey]
	if base != nil && base.DeletePending && !table.inodeOpen(key.Inode) {
		action.Object, action.Name, action.Remove = baseKey, base.DeleteName, true
		base.DeletePending, base.deleteCommitted = false, false
		if record != base {
			record.DeletePending, record.deleteCommitted = false, false
		}
		table.prune(baseKey)
	} else if key.Stream != "" && record.DeletePending && len(record.Opens) == 0 {
		action.Name, action.Remove = record.DeleteName, true
		record.DeletePending, record.deleteCommitted = false, false
	}
	table.prune(key)
	return action
}

func (table *Table) inodeOpen(inode smb.Inode) bool {
	for _, open := range table.opens {
		if open.Object.Inode == inode {
			return true
		}
	}
	return false
}
