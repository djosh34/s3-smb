package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	leaseOplockLevel     uint8  = 0xff
	leaseBreakInProgress uint32 = 0x02
	leaseParentKeySet    uint32 = 0x04
)

func decodeCreateLease(create wire.CreateRequest) (*wire.LeaseContext, smb.Status) {
	var lease *wire.LeaseContext
	for _, context := range create.Contexts {
		if context.Name != "RqLs" {
			continue
		}
		if lease != nil {
			return nil, smb.StatusInvalidParameter
		}
		decoded, err := wire.DecodeLeaseContext(context)
		if err != nil {
			return nil, smb.StatusInvalidParameter
		}
		lease = &decoded
	}
	return lease, smb.StatusSuccess
}

func createLeaseRequest(request RequestContext, create wire.CreateRequest, resolved smb.Resolved) state.Lease {
	lease, status := decodeCreateLease(create)
	if status != smb.StatusSuccess || lease == nil || lease.Version != 2 || create.OplockLevel != leaseOplockLevel ||
		lease.State&smb.LeaseRead == 0 || resolved.Attr.Kind != smb.KindFile || resolved.Object.Stream != "" {
		return state.Lease{}
	}
	leaseState := lease.State & (smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle)
	// RW is not a grant we support. R is its largest supported subset.
	if leaseState&smb.LeaseHandle == 0 {
		leaseState &^= smb.LeaseWrite
	}
	result := state.Lease{ClientGUID: request.Session.ClientGUID, Key: state.GUID(lease.Key), State: leaseState, Epoch: lease.Epoch}
	if lease.Flags&leaseParentKeySet != 0 {
		result.ParentKey = state.GUID(lease.ParentKey)
	}
	return result
}

// MS-FSA 2.1.4.12 breaks W for OPEN and all caching for overwrite/supersede.
// We also remove R on writer/append opens, before any WRITE can change data.
// That early break keeps H for ordinary opens; only a share conflict or a
// destructive disposition removes H.
func createLeaseTarget(create wire.CreateRequest, granted uint32) uint32 {
	if create.Disposition == fileSupersede || create.Disposition == fileOverwrite || create.Disposition == fileOverwriteIf {
		return 0
	}
	if granted&(fileWriteData|fileAppendData) != 0 {
		return smb.LeaseHandle
	}
	return smb.LeaseRead | smb.LeaseHandle
}

type createLeaseWait struct {
	objects []smb.ObjectKey
	client  state.GUID
	key     state.GUID
	target  uint32
	sharing bool
}

func (*createLeaseWait) Error() string { return "CREATE requires a lease break" }

// runCreateWithLeases releases the namespace guard before sending or waiting.
// The next attempt resolves the name again under its guard. H holders can CLOSE
// cached handles while we wait, and a changed name never selects a stale inode.
func runCreateWithLeases(ctx context.Context, request RequestContext, create wire.CreateRequest, granted uint32) (reply, error) {
	sharingRetried := false
	for {
		if err := ctx.Err(); err != nil {
			return reply{}, err
		}
		result, unlock, err := createLocked(ctx, request, create, granted)
		if unlock != nil {
			unlock()
		}
		var wait *createLeaseWait
		if !errors.As(err, &wait) {
			return result, err
		}
		if wait.sharing {
			if sharingRetried {
				return reply{status: smb.StatusSharingViolation}, nil
			}
			sharingRetried = true
		}
		for _, object := range wait.objects {
			if err := request.server.BreakLeases(ctx, object, wait.client, wait.key, wait.target); err != nil {
				return reply{}, err
			}
		}
	}
}

func reserveCreateLease(request RequestContext, create wire.CreateRequest, resolved smb.Resolved, open state.OpenRequest) (state.Reservation, smb.Status, error) {
	reservation, status := request.Opens.Reserve(open)
	lease := createLeaseRequest(request, create, resolved)
	target := createLeaseTarget(create, open.GrantedAccess)
	if status == smb.StatusSharingViolation {
		objects := request.Opens.SharingHandleLeases(open)
		if len(objects) != 0 {
			// MS-FSA OPEN_BREAK_H removes H on the conflicting stream's leases.
			// W also goes for a new open; a writer or overwrite removes R.
			return 0, status, &createLeaseWait{objects: objects, target: target &^ smb.LeaseHandle, sharing: true}
		}
	}
	if status != smb.StatusSuccess {
		return reservation, status, nil
	}
	_, status = request.Opens.PrepareLease(reservation, lease)
	var err error
	if status == smb.StatusSuccess && request.Opens.LeasesNeedBreak(resolved.Object, lease.ClientGUID, lease.Key, target) {
		err = &createLeaseWait{objects: []smb.ObjectKey{resolved.Object}, client: lease.ClientGUID, key: lease.Key, target: target}
	}
	if status != smb.StatusSuccess || err != nil {
		if aborted := request.Opens.Abort(reservation); aborted != smb.StatusSuccess {
			return 0, status, fmt.Errorf("abort lease reservation: status %#x", aborted)
		}
		return 0, status, err
	}
	return reservation, status, nil
}

func grantCreateLease(request RequestContext, create wire.CreateRequest, resolved smb.Resolved, reservation state.Reservation, grant *state.Grant) smb.Status {
	lease, status := request.Opens.PrepareLease(reservation, createLeaseRequest(request, create, resolved))
	if status == smb.StatusSuccess {
		grant.Lease = lease
	}
	return status
}

func createLeaseResponse(request RequestContext, create wire.CreateRequest, resolved smb.Resolved, open state.Open, response *wire.CreateResponse) error {
	lease, status := decodeCreateLease(create)
	if status != smb.StatusSuccess {
		return fmt.Errorf("decode response lease: status %#x", status)
	}
	if lease == nil || lease.Version != 2 || create.OplockLevel != leaseOplockLevel || resolved.Attr.Kind != smb.KindFile || resolved.Object.Stream != "" {
		return nil
	}
	if open.LeaseKey != (state.GUID{}) {
		return appendCreateLease(request, open, response)
	}
	result := wire.LeaseContext{Version: 2, Key: lease.Key, Epoch: lease.Epoch}
	if lease.Flags&leaseParentKeySet != 0 && lease.ParentKey != [16]byte{} {
		result.ParentKey, result.Flags = lease.ParentKey, leaseParentKeySet
	}
	context, err := wire.EncodeLeaseContext(result)
	if err != nil {
		return err
	}
	response.Contexts = append(response.Contexts, context)
	return nil
}

// appendCreateLease also serves replay and reconnect responses. The table, not
// the new request's epoch or parent key, supplies the current shared lease.
func appendCreateLease(request RequestContext, open state.Open, response *wire.CreateResponse) error {
	if open.LeaseKey == (state.GUID{}) {
		return nil
	}
	current, exists := request.Opens.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists {
		return errors.New("committed CREATE lease is missing")
	}
	result := wire.LeaseContext{
		Version: 2, Key: [16]byte(current.Key), State: current.State,
		Epoch: current.Epoch, ParentKey: [16]byte(current.ParentKey),
	}
	if current.Breaking {
		result.Flags |= leaseBreakInProgress
	}
	if current.ParentKey != (state.GUID{}) {
		result.Flags |= leaseParentKeySet
	}
	context, err := wire.EncodeLeaseContext(result)
	if err != nil {
		return err
	}
	response.OplockLevel = leaseOplockLevel
	response.Contexts = append(response.Contexts, context)
	return nil
}
