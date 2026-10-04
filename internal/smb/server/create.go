package server

import (
	"context"
	"errors"
	"fmt"
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
	fileNonDirectoryFile uint32 = 0x00000040
	fileDeleteOnClose    uint32 = 0x00001000
	fileOpenByFileID     uint32 = 0x00002000
)

func decodeCreate(message wire.Message) (wire.CreateRequest, smb.Status) {
	create, err := wire.DecodeCreateRequest(message)
	if err != nil {
		return create, smb.StatusInvalidParameter
	}
	if create.Options&fileOpenByFileID != 0 {
		return create, smb.StatusNotSupported
	}
	if create.Disposition > fileOverwriteIf || create.ShareAccess & ^uint32(7) != 0 || create.Options&(fileDirectoryFile|fileNonDirectoryFile) == fileDirectoryFile|fileNonDirectoryFile {
		return create, smb.StatusInvalidParameter
	}
	return create, smb.StatusSuccess
}

// expandCreateAccess removes generic bits before storing the granted mask.
func expandCreateAccess(desired uint32) uint32 {
	granted := desired &^ 0xf2000000
	if desired&0x80000000 != 0 {
		granted |= 0x00120089 // FILE_GENERIC_READ.
	}
	if desired&0x40000000 != 0 {
		granted |= 0x00120116 // FILE_GENERIC_WRITE.
	}
	if desired&0x20000000 != 0 {
		granted |= 0x001200a0 // FILE_GENERIC_EXECUTE.
	}
	if desired&0x12000000 != 0 { // GENERIC_ALL or MAXIMUM_ALLOWED.
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
	case fileSupersede, fileOverwrite, fileOverwriteIf:
		if resolved.Attr.Kind == smb.KindDirectory {
			return 0, false, smb.StatusFileIsADirectory
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
	default:
		return 0, false, smb.StatusInvalidParameter
	}
}

func reserveCreate(request RequestContext, create wire.CreateRequest, resolved smb.Resolved, granted uint32) (state.Reservation, smb.Status) {
	open := state.OpenRequest{
		Object: resolved.Object, Binding: request.Binding(), User: request.Session.User,
		Share: request.Tree.Share, ClientGUID: request.Session.ClientGUID,
		GrantedAccess: granted, Sharing: state.ShareMode(create.ShareAccess & 7),
	}
	if create.Disposition == fileSupersede || create.Options&fileDeleteOnClose != 0 {
		open.SharingIntent |= state.RightDelete
	}
	return request.Opens.Reserve(open)
}

func createGrant(create wire.CreateRequest, resolved smb.Resolved, handle smb.Handle) state.Grant {
	return state.Grant{
		Handle: handle, Directory: resolved.Attr.Kind == smb.KindDirectory,
		DeleteOnClose: create.Options&fileDeleteOnClose != 0, DeleteName: resolved.Name,
	}
}

func createStorageAccess(granted uint32, destructive bool) smb.Access {
	var access smb.Access
	if granted&fileReadData != 0 {
		access |= smb.AccessRead
	}
	if destructive || granted&(fileWriteData|fileAppendData) != 0 {
		access |= smb.AccessWrite
	}
	return access
}

func handleCreate(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	create, status := decodeCreate(message)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	granted := expandCreateAccess(create.DesiredAccess)
	if status = checkDeleteOnClose(create.Options, granted); status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	result, unlock, err := createLocked(ctx, request, create, granted)
	if unlock != nil {
		unlock()
	}
	return result, err
}

// createLocked holds the parent from selection through grant publication. The
// caller releases unlock even on a sharing violation, before waiting or retrying.
func createLocked(ctx context.Context, request RequestContext, create wire.CreateRequest, granted uint32) (reply, func(), error) {
	resolved, unlock, err := lookupLocked(ctx, request, create.Name)
	if err != nil {
		return reply{}, nil, err
	}
	result, err := createSelected(ctx, request, create, resolved, granted)
	return result, unlock, err
}

func createSelected(ctx context.Context, request RequestContext, create wire.CreateRequest, resolved smb.Resolved, granted uint32) (result reply, resultErr error) {
	var err error
	action, destructive, status := createDisposition(create, resolved, granted)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	if !resolved.Exists {
		kind := smb.KindFile
		if create.Options&fileDirectoryFile != 0 {
			kind = smb.KindDirectory
		}
		resolved, err = request.Storage.Create(ctx, resolved.Name, kind)
		if err != nil {
			return reply{}, err
		}
	}
	reservation, status := reserveCreate(request, create, resolved, granted)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
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
		return reply{}, err
	}
	if destructive {
		if truncateErr := request.Storage.Truncate(ctx, handle, 0); truncateErr != nil {
			return reply{}, truncateErr
		}
	}
	if action != 1 && create.FileAttributes != 0 {
		attributes := create.FileAttributes
		if action == 3 {
			attributes |= resolved.Attr.Attributes
		}
		if attrErr := request.Storage.SetAttr(ctx, resolved.Object, smb.AttrChange{Attributes: &attributes}); attrErr != nil {
			return reply{}, attrErr
		}
	}
	attr, err := request.Storage.GetAttr(ctx, resolved.Object)
	if err != nil {
		return reply{}, err
	}
	response, err := createResponse(attr, action)
	if err != nil {
		return reply{}, err
	}
	open, status := request.Opens.Commit(reservation, createGrant(create, resolved, handle))
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	committed = true
	response.ID = wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}
	body, err := wire.EncodeCreateResponse(response)
	if err != nil {
		return reply{}, errors.Join(err, closeFailedCreate(context.WithoutCancel(ctx), request, open))
	}
	return reply{body: body, fileID: response.ID}, nil
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
		return errors.Join(closeErr, request.Storage.Remove(ctx, action.Name, action.Object.Inode))
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
