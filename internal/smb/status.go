package smb

import (
	"context"
	"errors"
	"io"
)

// Status is an NTSTATUS value carried in an SMB response header.
type Status uint32

// Status codes used by the new server, from MS-ERREF. Unsupported operations
// return a status and keep the connection; invalid framing or authentication
// of a packet is handled by the connection layer, not this map.
const (
	StatusSuccess                Status = 0x00000000
	StatusPending                Status = 0x00000103
	StatusBufferOverflow         Status = 0x80000005
	StatusNoMoreFiles            Status = 0x80000006
	StatusUnsuccessful           Status = 0xc0000001
	StatusNotImplemented         Status = 0xc0000002
	StatusInvalidInfoClass       Status = 0xc0000003
	StatusInfoLengthMismatch     Status = 0xc0000004
	StatusInvalidHandle          Status = 0xc0000008
	StatusInvalidParameter       Status = 0xc000000d
	StatusNoSuchFile             Status = 0xc000000f
	StatusInvalidDeviceRequest   Status = 0xc0000010
	StatusEndOfFile              Status = 0xc0000011
	StatusMoreProcessingRequired Status = 0xc0000016
	StatusAccessDenied           Status = 0xc0000022
	StatusBufferTooSmall         Status = 0xc0000023
	StatusObjectNameInvalid      Status = 0xc0000033
	StatusObjectNameNotFound     Status = 0xc0000034
	StatusObjectNameCollision    Status = 0xc0000035
	StatusObjectPathNotFound     Status = 0xc000003a
	StatusSharingViolation       Status = 0xc0000043
	StatusFileLockConflict       Status = 0xc0000054
	StatusLockNotGranted         Status = 0xc0000055
	StatusDeletePending          Status = 0xc0000056
	StatusLogonFailure           Status = 0xc000006d
	StatusDiskFull               Status = 0xc000007f
	StatusInsufficientResources  Status = 0xc000009a
	StatusMediaWriteProtected    Status = 0xc00000a2
	StatusFileIsADirectory       Status = 0xc00000ba
	StatusNotSupported           Status = 0xc00000bb
	StatusBadNetworkName         Status = 0xc00000cc
	StatusInternalError          Status = 0xc00000e5
	StatusDirectoryNotEmpty      Status = 0xc0000101
	StatusNotADirectory          Status = 0xc0000103
	StatusCancelled              Status = 0xc0000120
	StatusFileClosed             Status = 0xc0000128
	StatusIOTimeout              Status = 0xc00000b5
	StatusIODeviceError          Status = 0xc0000185
	StatusDuplicateObjectID      Status = 0xc000022a
	StatusUserSessionDeleted     Status = 0xc0000203
	StatusNetworkNameDeleted     Status = 0xc00000c9
	StatusNetworkSessionExpired  Status = 0xc000035c
	StatusRangeNotLocked         Status = 0xc000007e
	StatusFileTooLarge           Status = 0xc0000904
)

// ErrorKind is a storage error category. The adapter translates errno at its
// seam, where it can distinguish missing leaf from missing ancestor and an
// identity mismatch from an I/O error. Wrap with fmt.Errorf and %w to retain
// this category. Backend diagnostics must be logged separately or joined.
type ErrorKind string

// Adapter error kinds. ErrIdentityChanged means a namespace expectation failed;
// the server may repeat lookup/check before replying, but must not mutate the
// newly found object using checks made for the old one.
const (
	ErrNameNotFound      ErrorKind = "object name not found"
	ErrPathNotFound      ErrorKind = "object path not found"
	ErrNameCollision     ErrorKind = "object name collision"
	ErrInvalidName       ErrorKind = "invalid object name"
	ErrAccessDenied      ErrorKind = "access denied"
	ErrReadOnly          ErrorKind = "read-only storage"
	ErrInvalidHandle     ErrorKind = "invalid storage handle"
	ErrInvalidParameter  ErrorKind = "invalid storage parameter"
	ErrNotDirectory      ErrorKind = "not a directory"
	ErrIsDirectory       ErrorKind = "object is a directory"
	ErrDirectoryNotEmpty ErrorKind = "directory not empty"
	ErrDiskFull          ErrorKind = "storage full"
	ErrFileTooLarge      ErrorKind = "object exceeds size limit"
	ErrNotSupported      ErrorKind = "storage operation not supported"
	ErrIdentityChanged   ErrorKind = "namespace identity changed"
	ErrIO                ErrorKind = "storage I/O failure"
	ErrResources         ErrorKind = "storage resources exhausted"
)

// Error returns the category's diagnostic text.
func (kind ErrorKind) Error() string {
	return string(kind)
}

// StatusFromError maps a storage error, including wrappers, to NTSTATUS. Nil is
// success; unknown errors are internal errors, never success or a disconnect.
// io.EOF maps to end-of-file only after the handler checks the read byte count.
// Context errors take precedence over categories in an errors.Join value;
// an explicit storage category takes precedence over a backend EOF cause.
func StatusFromError(err error) Status {
	if err == nil {
		return StatusSuccess
	}
	if errors.Is(err, context.Canceled) {
		return StatusCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusIOTimeout
	}
	var kind ErrorKind
	if !errors.As(err, &kind) {
		if errors.Is(err, io.EOF) {
			return StatusEndOfFile
		}
		return StatusInternalError
	}
	switch kind {
	case ErrNameNotFound:
		return StatusObjectNameNotFound
	case ErrPathNotFound:
		return StatusObjectPathNotFound
	case ErrNameCollision:
		return StatusObjectNameCollision
	case ErrInvalidName:
		return StatusObjectNameInvalid
	case ErrAccessDenied:
		return StatusAccessDenied
	case ErrReadOnly:
		return StatusMediaWriteProtected
	case ErrInvalidHandle:
		return StatusInvalidHandle
	case ErrInvalidParameter:
		return StatusInvalidParameter
	case ErrNotDirectory:
		return StatusNotADirectory
	case ErrIsDirectory:
		return StatusFileIsADirectory
	case ErrDirectoryNotEmpty:
		return StatusDirectoryNotEmpty
	case ErrDiskFull:
		return StatusDiskFull
	case ErrFileTooLarge:
		return StatusFileTooLarge
	case ErrNotSupported:
		return StatusNotSupported
	case ErrIdentityChanged:
		return StatusObjectNameNotFound
	case ErrIO:
		return StatusIODeviceError
	case ErrResources:
		return StatusInsufficientResources
	}
	return StatusInternalError
}
