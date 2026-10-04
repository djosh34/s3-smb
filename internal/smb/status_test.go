package smb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestStatusFromError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		name string
		want smb.Status
	}{
		{nil, "success", smb.StatusSuccess},
		{smb.ErrNameNotFound, "leaf", smb.StatusObjectNameNotFound},
		{smb.ErrPathNotFound, "ancestor", smb.StatusObjectPathNotFound},
		{smb.ErrNameCollision, "collision", smb.StatusObjectNameCollision},
		{smb.ErrInvalidName, "name", smb.StatusObjectNameInvalid},
		{smb.ErrAccessDenied, "access", smb.StatusAccessDenied},
		{smb.ErrReadOnly, "read-only", smb.StatusMediaWriteProtected},
		{smb.ErrInvalidHandle, "handle", smb.StatusInvalidHandle},
		{smb.ErrInvalidParameter, "parameter", smb.StatusInvalidParameter},
		{smb.ErrNotDirectory, "not-directory", smb.StatusNotADirectory},
		{smb.ErrIsDirectory, "directory", smb.StatusFileIsADirectory},
		{smb.ErrDirectoryNotEmpty, "not-empty", smb.StatusDirectoryNotEmpty},
		{smb.ErrDiskFull, "full", smb.StatusDiskFull},
		{smb.ErrFileTooLarge, "too-large", smb.StatusFileTooLarge},
		{smb.ErrNotSupported, "unsupported", smb.StatusNotSupported},
		{smb.ErrIdentityChanged, "identity", smb.StatusObjectNameNotFound},
		{smb.ErrIO, "io", smb.StatusIODeviceError},
		{errors.Join(smb.ErrIO, errors.New("backend detail")), "joined-backend", smb.StatusIODeviceError},
		{errors.Join(smb.ErrIO, io.EOF), "classified-eof", smb.StatusIODeviceError},
		{smb.ErrResources, "resources", smb.StatusInsufficientResources},
		{context.Canceled, "cancel", smb.StatusCancelled},
		{context.DeadlineExceeded, "deadline", smb.StatusIOTimeout},
		{io.EOF, "eof", smb.StatusEndOfFile},
		{io.ErrUnexpectedEOF, "short-input", smb.StatusInternalError},
		{errors.New("backend detail"), "unknown", smb.StatusInternalError},
		{smb.ErrorKind("future category"), "unknown-kind", smb.StatusInternalError},
		{errors.Join(smb.ErrIO, context.Canceled), "cancel-precedence", smb.StatusCancelled},
		{errors.Join(smb.ErrIO, context.DeadlineExceeded), "deadline-precedence", smb.StatusIOTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := smb.StatusFromError(test.err); got != test.want {
				t.Fatalf("status = %#x, want %#x", got, test.want)
			}
			if test.err == nil {
				return
			}
			wrapped := fmt.Errorf("operation: %w", fmt.Errorf("backend: %w", test.err))
			if got := smb.StatusFromError(wrapped); got != test.want {
				t.Fatalf("wrapped status = %#x, want %#x", got, test.want)
			}
		})
	}
}
