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
		objects:      make(map[smb.Inode]*objectEntry),
		creates:      make(map[createIdentity]createEntry),
		leaseObjects: make(map[leaseIdentity]smb.Inode),
		cleaning:     make(map[GUID]int),
		failed:       make(map[GUID]bool),
		breakChanges: make(chan struct{}),
	}, nil
}

func identity(request OpenRequest) createIdentity {
	return createIdentity{user: request.User, share: request.Share, client: request.ClientGUID, create: request.CreateGUID}
}

func openRequest(open Open) OpenRequest {
	return OpenRequest{
		User: open.User, Share: open.Share, Object: open.Object, Binding: open.Binding,
		ClientGUID: open.ClientGUID, CreateGUID: open.CreateGUID,
		GrantedAccess: open.GrantedAccess, SharingIntent: open.SharingIntent, Sharing: open.Sharing,
	}
}

func sharingIntent(request OpenRequest) OpenRequest {
	if request.GrantedAccess&0x00000021 != 0 { // FILE_READ_DATA or FILE_EXECUTE.
		request.SharingIntent |= RightRead
	}
	if request.GrantedAccess&0x00000006 != 0 { // FILE_WRITE_DATA or FILE_APPEND_DATA.
		request.SharingIntent |= RightWrite
	}
	if request.GrantedAccess&deleteAccess != 0 {
		request.SharingIntent |= RightDelete
	}
	return request
}

func validBinding(binding Binding) bool {
	return binding.SessionID != 0 && binding.TreeID != 0
}

func availableID(id uint64) bool {
	return id < math.MaxUint64-1
}

// Reserve adds the granted mask's minimum sharing intent, then checks sharing
// and deletion before acquiring a CREATE reservation.
func (table *Table) Reserve(request OpenRequest) (Reservation, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if request.Object == 0 || !validBinding(request.Binding) || request.SharingIntent & ^allRights != 0 || Rights(request.Sharing) & ^allRights != 0 {
		return 0, smb.StatusInvalidParameter
	}
	request = sharingIntent(request)
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
	if left.Object != right.Object {
		return true
	}
	// MS-FSA 2.1.5.1.2.2 excludes metadata-only opens from both
	// directions of sharing.
	if left.SharingIntent == 0 || right.SharingIntent == 0 {
		return true
	}
	return left.SharingIntent & ^Rights(right.Sharing) == 0 && right.SharingIntent & ^Rights(left.Sharing) == 0
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

func (table *Table) deletePending(key smb.Inode) bool {
	record := table.objects[key]
	return record != nil && record.DeletePending
}

func (table *Table) object(key smb.Inode) *objectEntry {
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

// Commit replaces a reservation with an open, granting its lease and
// durability in the same step. Invalid grants leave it reserved.
func (table *Table) Commit(reservation Reservation, grant Grant) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	request, exists := table.reservations[reservation]
	if !exists {
		return Open{}, smb.StatusInvalidParameter
	}
	if table.deletePending(request.Object) {
		return Open{}, smb.StatusDeletePending
	}
	if status := table.validateGrant(request, reservation, grant); status != smb.StatusSuccess {
		return Open{}, status
	}
	if !availableID(table.nextPersistent) || !availableID(table.nextVolatile) {
		return Open{}, smb.StatusInsufficientResources
	}
	leaseKey, status := table.grantLease(request, reservation, grant.Lease)
	if status != smb.StatusSuccess {
		return Open{}, status
	}
	durable := grant.DurableTimeout > 0 && request.CreateGUID != (GUID{}) && leaseKey != (GUID{}) && table.objects[request.Object].lease.handle()
	if !durable {
		grant.DurableTimeout = 0
	}
	table.nextPersistent++
	table.nextVolatile++
	open := Open{
		Handle: grant.Handle, User: request.User, Share: request.Share, Object: request.Object,
		ID: FileID{Persistent: table.nextPersistent, Volatile: table.nextVolatile}, Binding: request.Binding,
		ClientGUID: request.ClientGUID, CreateGUID: request.CreateGUID, LeaseKey: leaseKey,
		GrantedAccess: request.GrantedAccess, SharingIntent: request.SharingIntent, Sharing: request.Sharing,
		DeleteOnClose: grant.DeleteOnClose, WriteThrough: grant.WriteThrough, Kind: grant.Kind,
		Durable: durable, DurableTimeout: grant.DurableTimeout,
	}
	table.releaseReservation(reservation, request)
	table.opens[open.ID.Persistent] = &openEntry{Open: open, deleteName: grant.DeleteName}
	record := table.object(request.Object)
	record.Opens = append(record.Opens, open.ID.Persistent)
	if request.CreateGUID != (GUID{}) {
		table.creates[identity(request)] = createEntry{persistent: open.ID.Persistent}
	}
	return open, smb.StatusSuccess
}

func validName(name smb.Name) bool {
	return name.Parent != 0 && name.Base != ""
}

func (table *Table) validateGrant(request OpenRequest, reservation Reservation, grant Grant) smb.Status {
	if grant.Handle == nil || grant.DurableTimeout < 0 || grant.DurableTimeout > smb.MaxDurableTimeout {
		return smb.StatusInvalidParameter
	}
	if grant.DeleteOnClose {
		if request.GrantedAccess&deleteAccess == 0 {
			return smb.StatusAccessDenied
		}
		if !validName(grant.DeleteName) {
			return smb.StatusInvalidParameter
		}
		deleting := request
		deleting.SharingIntent |= RightDelete
		if !table.sharingAllowed(deleting, 0, reservation) {
			return smb.StatusSharingViolation
		}
	}
	return smb.StatusSuccess
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

// DeletePending reports deletion of the object.
func (table *Table) DeletePending(key smb.Inode) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.deletePending(key)
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

// SetDelete requires delete access and compatible sharing. It changes this
// open's deletion intent, not another open's intent or an already closed intent.
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
		if !validName(name) {
			return smb.StatusInvalidParameter
		}
		request := openRequest(open.Open)
		request.SharingIntent |= RightDelete
		if !table.sharingAllowed(request, id.Persistent, 0) {
			return smb.StatusSharingViolation
		}
	}
	record := table.objects[open.Object]
	if pending && !record.DeletePending {
		record.DeleteName = name
	}
	open.dispositionPending = pending
	table.refreshDelete(record)
	return smb.StatusSuccess
}

func (table *Table) refreshDelete(record *objectEntry) {
	record.DeletePending = record.deleteCommitted
	for _, id := range record.Opens {
		if table.opens[id].dispositionPending {
			record.DeletePending = true
		}
	}
	if !record.DeletePending {
		record.DeleteName = smb.Name{}
	}
}

func (table *Table) prune(key smb.Inode) {
	record := table.objects[key]
	if record == nil || len(record.Opens) != 0 || record.lease != nil || record.DeletePending {
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
	action := CloseAction{FileID: open.ID, Handle: open.Handle, Object: key, ClientGUID: open.ClientGUID}
	table.cleaning[open.ClientGUID]++
	if open.DeleteOnClose || open.dispositionPending {
		if !record.DeletePending {
			record.DeleteName = open.deleteName
		}
		record.deleteCommitted = true
	}
	delete(table.opens, open.ID.Persistent)
	if open.CreateGUID != (GUID{}) {
		delete(table.creates, identity(openRequest(open.Open)))
	}
	record.Opens = slices.DeleteFunc(record.Opens, func(id uint64) bool { return id == open.ID.Persistent })
	table.releaseLease(record)
	table.refreshDelete(record)
	if record.DeletePending && len(record.Opens) == 0 {
		action.Name, action.Remove = record.DeleteName, true
		record.removalPending = true
	}
	table.prune(key)
	return action
}

// CleanupDone ends the cleanup of a close action. A failed cleanup keeps its
// client present for the one-client rule until the server restarts, since
// its storage may still be in use.
func (table *Table) CleanupDone(action CloseAction, err error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.cleaning[action.ClientGUID]--; table.cleaning[action.ClientGUID] <= 0 {
		delete(table.cleaning, action.ClientGUID)
	}
	if err != nil {
		table.failed[action.ClientGUID] = true
	}
}

// CompleteDelete releases the delete-pending barrier after a Remove action,
// whether cleanup succeeded or failed. Until then Reserve and Commit reject
// this object, including during bulk-close cleanup before its namespace lock.
func (table *Table) CompleteDelete(object smb.Inode) {
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.objects[object]
	if record == nil || !record.removalPending {
		return
	}
	record.removalPending = false
	record.deleteCommitted = false
	record.DeletePending = false
	record.DeleteName = smb.Name{}
	table.prune(object)
}

// InodeOpen reports whether an inode has any open or sharing reservation,
// including detached durable opens.
func (table *Table) InodeOpen(inode smb.Inode) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	if record := table.objects[inode]; record != nil && len(record.Opens) != 0 {
		return true
	}
	for _, request := range table.reservations {
		if request.Object == inode {
			return true
		}
	}
	return false
}
