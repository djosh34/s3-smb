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

// createContexts holds the lease and durable handle contexts of one CREATE.
type createContexts struct {
	lease           *wire.LeaseContext
	durable         *wire.DurableRequest
	reconnect       *wire.DurableReconnect
	legacyRequest   bool
	legacyReconnect bool
}

// decodeCreateContexts refuses malformed or duplicate lease and durable
// contexts, and a durable v2 request mixed with a durable v1 context.
func decodeCreateContexts(create wire.CreateRequest) (createContexts, error) {
	var result createContexts
	for _, c := range create.Contexts {
		var err error
		switch c.Name {
		case "RqLs":
			if result.lease != nil {
				return result, errors.New("duplicate lease context")
			}
			result.lease, err = decoded(wire.DecodeLeaseContext(c))
		case "DH2Q", "DH2C":
			if result.durable != nil || result.reconnect != nil {
				return result, errors.New("duplicate durable context")
			}
			if c.Name == "DH2Q" {
				result.durable, err = decoded(wire.DecodeDurableRequest(c))
			} else {
				result.reconnect, err = decoded(wire.DecodeDurableReconnect(c))
			}
		case "DHnQ":
			result.legacyRequest = true
		case "DHnC":
			result.legacyReconnect = true
		}
		if err != nil {
			return result, err
		}
	}
	if result.durable != nil && result.durable.CreateGUID == ([16]byte{}) {
		return result, errors.New("durable request has no CREATE GUID")
	}
	if result.durable != nil && (result.legacyRequest || result.legacyReconnect) {
		return result, errors.New("durable v2 request conflicts with a durable v1 context")
	}
	return result, nil
}

func decoded[T any](value T, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// leaseRequest is the lease this CREATE asks for: a V2 lease on a regular
// unnamed file. Only R, RH and RWH are granted; RW falls back to R and a
// request without R asks for nothing, though it still joins a held lease of
// the same key.
func (contexts createContexts) leaseRequest(request RequestContext, create wire.CreateRequest, resolved smb.Resolved) state.Lease {
	lease := contexts.lease
	if lease == nil || lease.Version != 2 || create.OplockLevel != leaseOplockLevel || resolved.Attr.Kind != smb.KindFile || resolved.Object.Stream != "" {
		return state.Lease{}
	}
	leaseState := lease.State & (smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle)
	if leaseState&smb.LeaseRead == 0 {
		leaseState = 0
	} else if leaseState&smb.LeaseHandle == 0 {
		leaseState = smb.LeaseRead
	}
	result := state.Lease{ClientGUID: request.Session.ClientGUID, Key: state.GUID(lease.Key), State: leaseState, Epoch: lease.Epoch}
	if lease.Flags&leaseParentKeySet != 0 {
		result.ParentKey = state.GUID(lease.ParentKey)
	}
	return result
}

// createLeaseTarget is what another lease on the file may keep while this
// open exists (MS-FSA 2.1.4.12). Opens for attributes only break nothing,
// overwriting breaks everything, and other opens break W.
func createLeaseTarget(create wire.CreateRequest, granted uint32) uint32 {
	// FILE_READ_ATTRIBUTES, FILE_WRITE_ATTRIBUTES, READ_CONTROL and SYNCHRONIZE.
	if granted&^uint32(0x00120180) == 0 {
		return smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle
	}
	if create.Disposition == fileSupersede || create.Disposition == fileOverwrite || create.Disposition == fileOverwriteIf {
		return 0
	}
	return smb.LeaseRead | smb.LeaseHandle
}

// leaseBreak is a lease that must be broken before a CREATE can go on.
// Client and key name the opener's own lease, which is never broken.
type leaseBreak struct {
	object smb.ObjectKey
	client state.GUID
	key    state.GUID
	target uint32
}

// runCreate breaks a lease in the way of the CREATE, waits for its holder, and
// tries once more. Each attempt holds the parent guard only while it runs, so
// the holder can close its handles meanwhile. A lease still in the way after
// that gets SHARING_VIOLATION.
func runCreate(ctx context.Context, request RequestContext, create wire.CreateRequest, contexts createContexts, granted uint32) (reply, error) {
	result, conflict, err := createOnce(ctx, request, create, contexts, granted)
	if conflict == nil || err != nil {
		return result, err
	}
	if err = request.server.breakLease(ctx, *conflict); err != nil {
		return reply{}, err
	}
	result, conflict, err = createOnce(ctx, request, create, contexts, granted)
	if conflict != nil {
		return reply{status: smb.StatusSharingViolation}, err
	}
	return result, err
}

// reserveCreate reserves the open for sharing. It returns a lease to break
// instead when an H lease holds a conflicting open, or when another lease
// keeps rights this open takes away.
func reserveCreate(request RequestContext, open state.OpenRequest, lease state.Lease, target uint32) (state.Reservation, *leaseBreak, smb.Status, error) {
	reservation, status := request.Opens.Reserve(open)
	if status == smb.StatusSharingViolation {
		if object, found := request.Opens.SharingLease(open); found {
			return 0, &leaseBreak{object: object, target: target &^ smb.LeaseHandle}, status, nil
		}
	}
	if status != smb.StatusSuccess {
		return 0, nil, status, nil
	}
	if !request.Opens.LeaseNeedsBreak(open.Object, lease.ClientGUID, lease.Key, target) {
		return reservation, nil, status, nil
	}
	conflict := &leaseBreak{object: open.Object, client: lease.ClientGUID, key: lease.Key, target: target}
	if aborted := request.Opens.Abort(reservation); aborted != smb.StatusSuccess {
		return 0, nil, aborted, fmt.Errorf("abort CREATE reservation: status %#x", aborted)
	}
	return 0, conflict, status, nil
}

// appendCreateContexts adds the lease and durable contexts for open from a
// fresh snapshot, so the reply shows a break that started after the grant.
// A requested lease that was not granted is answered with lease state none.
func appendCreateContexts(request RequestContext, open state.Open, requested state.Lease, response *wire.CreateResponse) error {
	open, lease, status := request.Opens.LeaseForOpen(open.ID, request.Binding())
	if status != smb.StatusSuccess {
		return fmt.Errorf("find created open: status %#x", status)
	}
	if open.LeaseKey == (state.GUID{}) {
		lease = requested
		lease.State = 0
	}
	if lease.Key != (state.GUID{}) {
		result := wire.LeaseContext{Version: 2, Key: [16]byte(lease.Key), State: lease.State, Epoch: lease.Epoch, ParentKey: [16]byte(lease.ParentKey)}
		if lease.Breaking {
			result.Flags |= leaseBreakInProgress
		}
		if lease.ParentKey != (state.GUID{}) {
			result.Flags |= leaseParentKeySet
		}
		context, err := wire.EncodeLeaseContext(result)
		if err != nil {
			return err
		}
		response.Contexts = append(response.Contexts, context)
	}
	if open.LeaseKey != (state.GUID{}) {
		response.OplockLevel = leaseOplockLevel
	}
	if open.Durable {
		timeout := uint32(open.DurableTimeout.Milliseconds()) //nolint:gosec // Durable timeouts are at most 16 minutes.
		context, err := wire.EncodeDurableReply(wire.DurableReply{Timeout: timeout})
		if err != nil {
			return err
		}
		response.Contexts = append(response.Contexts, context)
	}
	return nil
}
