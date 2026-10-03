package smb2

import (
	"errors"
	"syscall"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
)

// statusFromError retains native errno semantics across wrapped adapter errors.
func statusFromError(err error) NtStatus {
	switch {
	case err == nil:
		return STATUS_SUCCESS
	case errors.Is(err, syscall.EBADF):
		return STATUS_INVALID_HANDLE
	case errors.Is(err, syscall.ENOENT), errors.Is(err, missingXattrError):
		return STATUS_OBJECT_NAME_NOT_FOUND
	case errors.Is(err, syscall.ENOTDIR):
		return STATUS_NOT_A_DIRECTORY
	case errors.Is(err, syscall.EISDIR):
		return STATUS_FILE_IS_A_DIRECTORY
	case errors.Is(err, syscall.EEXIST):
		return STATUS_OBJECT_NAME_COLLISION
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return STATUS_ACCESS_DENIED
	case errors.Is(err, syscall.EROFS):
		return STATUS_MEDIA_WRITE_PROTECTED
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return STATUS_DISK_FULL
	case errors.Is(err, syscall.E2BIG):
		return STATUS_EA_TOO_LARGE
	case errors.Is(err, syscall.ERANGE):
		return STATUS_BUFFER_TOO_SMALL
	case errors.Is(err, syscall.EINVAL):
		return STATUS_INVALID_PARAMETER
	case errors.Is(err, syscall.ENOTEMPTY):
		return STATUS_DIRECTORY_NOT_EMPTY
	default:
		return STATUS_IO_DEVICE_ERROR
	}
}

func (t *fileTree) lookupOpen(id *FileId) *Open {
	if id == nil || IsInvalidFileId(id) {
		return nil
	}
	open := t.conn.serverCtx.getOpen(id.HandleId())
	if open == nil || open.durableFileId != id.NodeId() || open.session != t.session || open.tree != &t.treeConn {
		return nil
	}
	return open
}

func (t *fileTree) sendError(ctx *compoundContext, pkt []byte, err error) error {
	rsp := new(ErrorResponse)
	PrepareResponse(rsp.Header(), pkt, uint32(statusFromError(err)))
	return t.conn.sendPacket(rsp, &t.treeConn, ctx)
}
