package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	fileSupersede uint32 = iota
	fileOpen
	fileCreateDisposition
	fileOpenIf
	fileOverwrite
	fileOverwriteIf
)

const (
	fileDirectoryFile    uint32 = 0x00000001
	fileWriteThrough     uint32 = 0x00000002
	fileNonDirectoryFile uint32 = 0x00000040
	fileDeleteOnClose    uint32 = 0x00001000
	fileOpenByFileID     uint32 = 0x00002000
	fileReserveOpfilter  uint32 = 0x00100000
)

func decodeCreate(message wire.Message) (wire.CreateRequest, createContexts, smb.Status) {
	create, err := wire.DecodeCreateRequest(message)
	if err != nil {
		return create, createContexts{}, smb.StatusInvalidParameter
	}
	if create.ImpersonationLevel > 3 {
		return create, createContexts{}, smb.StatusBadImpersonationLevel
	}
	if strings.HasPrefix(create.Name, "\\") || strings.HasPrefix(create.Name, "/") {
		return create, createContexts{}, smb.StatusInvalidParameter
	}
	if create.Options&(fileOpenByFileID|fileReserveOpfilter) != 0 {
		return create, createContexts{}, smb.StatusNotSupported
	}
	if create.Disposition > fileOverwriteIf || create.ShareAccess & ^uint32(7) != 0 || create.Options&(fileDirectoryFile|fileNonDirectoryFile) == fileDirectoryFile|fileNonDirectoryFile {
		return create, createContexts{}, smb.StatusInvalidParameter
	}
	if create.Options&fileDirectoryFile != 0 && create.Disposition != fileCreateDisposition && create.Disposition != fileOpen && create.Disposition != fileOpenIf {
		return create, createContexts{}, smb.StatusInvalidParameter
	}
	contexts, err := decodeCreateContexts(create)
	if err != nil {
		return create, contexts, smb.StatusInvalidParameter
	}
	contexts.aapl, err = createAAPLContexts(create.Contexts)
	if err != nil {
		return create, contexts, smb.StatusInvalidParameter
	}
	return create, contexts, smb.StatusSuccess
}

// expandCreateAccess removes generic bits before storing the granted mask.
func expandCreateAccess(desired uint32) uint32 {
	granted := desired &^ (genericRead | genericWrite | genericExecute | genericAll | maximumAllowed)
	if desired&genericRead != 0 {
		granted |= fileGenericRead
	}
	if desired&genericWrite != 0 {
		granted |= fileGenericWrite
	}
	if desired&genericExecute != 0 {
		granted |= fileGenericExecute
	}
	if desired&(genericAll|maximumAllowed) != 0 {
		granted |= fileAllAccess
	}
	return granted
}

func createDisposition(create wire.CreateRequest, resolved smb.Resolved, granted uint32) (action uint32, destructive bool, status smb.Status) {
	if !resolved.Exists {
		if create.Disposition == fileOpen || create.Disposition == fileOverwrite {
			return 0, false, smb.StatusObjectNameNotFound
		}
		return 2, false, smb.StatusSuccess // FILE_CREATED.
	}
	if create.Disposition == fileCreateDisposition && create.Options&fileDirectoryFile != 0 {
		return 0, false, smb.StatusObjectNameCollision
	}
	if create.Options&fileDirectoryFile != 0 && resolved.Attr.Kind != smb.KindDirectory {
		return 0, false, smb.StatusNotADirectory
	}
	if create.Options&fileNonDirectoryFile != 0 && resolved.Attr.Kind == smb.KindDirectory {
		return 0, false, smb.StatusFileIsADirectory
	}
	switch create.Disposition {
	case fileCreateDisposition:
		return 0, false, smb.StatusObjectNameCollision
	case fileOpen, fileOpenIf:
		return 1, false, smb.StatusSuccess // FILE_OPENED.
	}
	// Supersede and overwrite replace the data of an existing file.
	if resolved.Attr.Kind == smb.KindDirectory {
		return 0, false, smb.StatusFileIsADirectory
	}
	if resolved.Attr.Attributes&0x6&^create.FileAttributes != 0 {
		// MS-FSA refuses destructive opens that omit an existing HIDDEN or
		// SYSTEM attribute, before truncation.
		return 0, false, smb.StatusAccessDenied
	}
	if create.Disposition == fileSupersede {
		if granted&fileDelete == 0 {
			return 0, false, smb.StatusAccessDenied
		}
		return 0, true, smb.StatusSuccess // FILE_SUPERSEDED.
	}
	if granted&fileWriteData == 0 {
		return 0, false, smb.StatusAccessDenied
	}
	return 3, true, smb.StatusSuccess // FILE_OVERWRITTEN.
}

func openRequestForCreate(request RequestContext, create wire.CreateRequest, contexts createContexts, object smb.Inode, granted uint32) state.OpenRequest {
	open := state.OpenRequest{
		Object: object, Binding: request.Binding(), User: request.Session.User,
		Share: request.Tree.Share, ClientGUID: request.Session.ClientGUID,
		GrantedAccess: granted, Sharing: state.ShareMode(create.ShareAccess & 7),
	}
	if create.Disposition == fileSupersede || create.Options&fileDeleteOnClose != 0 {
		open.SharingIntent |= state.RightDelete
	}
	if contexts.durable != nil {
		open.CreateGUID = contexts.durable.CreateGUID
	}
	return open
}

func createStorageAccess(granted uint32, destructive bool) smb.Access {
	var access smb.Access
	if granted&(fileReadData|fileExecute) != 0 {
		access |= smb.AccessRead
	}
	if destructive || granted&fileWriteData != 0 {
		access |= smb.AccessWrite
	}
	if granted&fileAppendData != 0 && granted&fileWriteData == 0 {
		// Destructive dispositions may truncate, but later writes remain append-only.
		access |= smb.AccessAppend
	}
	return access
}

// handleCreate refuses a DH2Q whose CREATE GUID is in use before it touches
// the namespace. A marked replay gets the same answer: without multichannel,
// macOS does not replay a CREATE.
func handleCreate(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	create, contexts, status := decodeCreate(message)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	if contexts.reconnect != nil || contexts.legacyReconnect {
		return reconnectCreate(ctx, request, create, contexts)
	}
	granted := expandCreateAccess(create.DesiredAccess)
	if status = checkDeleteOnClose(create.Options, granted); status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	if contexts.durable != nil {
		if _, status = request.Opens.LookupCreate(openRequestForCreate(request, create, contexts, 0, granted)); status != smb.StatusObjectNameNotFound {
			return reply{status: smb.StatusDuplicateObjectID}, nil
		}
	}
	return runCreate(ctx, request, create, contexts, granted)
}

// createOnce holds the parent guard from name selection through publication.
func createOnce(ctx context.Context, request RequestContext, create wire.CreateRequest, contexts createContexts, granted uint32) (reply, *leaseBreak, error) {
	resolved, unlock, err := lookupLocked(ctx, request, create.Name)
	if err != nil {
		return reply{}, nil, err
	}
	defer unlock()
	return createSelected(ctx, request, create, contexts, resolved, granted)
}

func createSelected(ctx context.Context, request RequestContext, create wire.CreateRequest, contexts createContexts, resolved smb.Resolved, granted uint32) (result reply, conflict *leaseBreak, resultErr error) {
	action, destructive, status := createDisposition(create, resolved, granted)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil, nil
	}
	if !resolved.Exists {
		resolved.Attr.Kind = smb.KindFile
		if create.Options&fileDirectoryFile != 0 {
			resolved.Attr.Kind = smb.KindDirectory
		}
	}
	lease := contexts.leaseRequest(request, create, resolved)
	if request.Opens.LeaseKeyElsewhere(resolved.Object, lease.ClientGUID, lease.Key) {
		return reply{status: smb.StatusInvalidParameter}, nil, nil
	}
	if !resolved.Exists {
		var err error
		resolved, err = request.Storage.Create(ctx, resolved.Name, resolved.Attr.Kind)
		if err != nil {
			return reply{}, nil, err
		}
	}
	open := openRequestForCreate(request, create, contexts, resolved.Object, granted)
	reservation, conflict, status, err := reserveCreate(request, open, lease, createLeaseTarget(create, granted))
	if conflict != nil || status != smb.StatusSuccess || err != nil {
		return reply{status: status}, conflict, err
	}
	var handle smb.Handle
	committed := false
	defer func() {
		if committed {
			return
		}
		if abortStatus := request.Opens.Abort(reservation); abortStatus != smb.StatusSuccess {
			resultErr = errors.Join(resultErr, fmt.Errorf("abort CREATE reservation: status %#x", abortStatus))
		}
		if handle != nil {
			resultErr = errors.Join(resultErr, request.Storage.Close(context.WithoutCancel(ctx), handle))
		}
	}()
	handle, err = request.Storage.Open(ctx, resolved.Object, createStorageAccess(granted, destructive))
	if err != nil {
		return reply{}, nil, err
	}
	if destructive {
		if truncateErr := request.Storage.Truncate(ctx, handle, 0); truncateErr != nil {
			return reply{}, nil, truncateErr
		}
	}
	if attrErr := setCreateAttributes(ctx, request.Storage, create, resolved, action); attrErr != nil {
		return reply{}, nil, attrErr
	}
	attr, err := request.Storage.GetAttr(ctx, resolved.Object)
	if err != nil {
		return reply{}, nil, err
	}
	response, err := createResponse(attr, action)
	if err != nil {
		return reply{}, nil, err
	}
	created, status := request.Opens.Commit(reservation, state.Grant{
		Handle: handle, DeleteName: resolved.Name, Lease: lease, DurableTimeout: contexts.durableTimeout(resolved),
		DeleteOnClose: create.Options&fileDeleteOnClose != 0, WriteThrough: create.Options&fileWriteThrough != 0,
		Kind: attr.Kind,
	})
	if status != smb.StatusSuccess {
		return reply{status: status}, nil, nil
	}
	committed = true
	response.ID = wire.FileID(created.ID)
	response.Contexts = contexts.aapl
	if err = appendCreateContexts(request, created, lease, true, &response); err != nil {
		return reply{}, nil, errors.Join(err, closeFailedCreate(context.WithoutCancel(ctx), request, created))
	}
	body, err := wire.EncodeCreateResponse(response)
	if err != nil {
		return reply{}, nil, errors.Join(err, closeFailedCreate(context.WithoutCancel(ctx), request, created))
	}
	return reply{body: body, fileID: response.ID}, nil, nil
}

// A failed reply has never exposed this grant to a client. Its parent is still
// guarded, so storage cleanup must not try to acquire that guard again.
func closeFailedCreate(ctx context.Context, request RequestContext, open state.Open) error {
	action, status := request.Opens.Close(open.ID, request.Binding())
	if status != smb.StatusSuccess {
		return fmt.Errorf("close failed CREATE: status %#x", status)
	}
	closeErr := request.Storage.Close(ctx, action.Handle)
	if action.Remove {
		defer request.Opens.CompleteDelete(action.Object)
		return errors.Join(closeErr, request.Storage.Remove(ctx, action.Name, action.Object))
	}
	return closeErr
}

func createResponse(attr smb.Attr, action uint32) (wire.CreateResponse, error) {
	times, err := createTimes(attr)
	return wire.CreateResponse{
		Action: action, Created: times[0], Accessed: times[1], Modified: times[2], Changed: times[3],
		AllocationSize: attr.AllocationSize, Size: attr.Size, Attributes: attr.Attributes,
	}, err
}

func createTimes(attr smb.Attr) ([4]uint64, error) {
	var times [4]uint64
	for i, value := range []time.Time{attr.Created, attr.Accessed, attr.Modified, attr.Changed} {
		encoded, err := wire.EncodeFiletime(value)
		if err != nil {
			return times, err
		}
		times[i] = uint64(encoded)
	}
	return times, nil
}
