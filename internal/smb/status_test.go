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
		name string
		err  error
		want smb.Status
	}{
		{"success", nil, smb.StatusSuccess},
		{"leaf", smb.ErrNameNotFound, smb.StatusObjectNameNotFound},
		{"ancestor", smb.ErrPathNotFound, smb.StatusObjectPathNotFound},
		{"collision", smb.ErrNameCollision, smb.StatusObjectNameCollision},
		{"name", smb.ErrInvalidName, smb.StatusObjectNameInvalid},
		{"access", smb.ErrAccessDenied, smb.StatusAccessDenied},
		{"read-only", smb.ErrReadOnly, smb.StatusMediaWriteProtected},
		{"handle", smb.ErrInvalidHandle, smb.StatusInvalidHandle},
		{"parameter", smb.ErrInvalidParameter, smb.StatusInvalidParameter},
		{"not-directory", smb.ErrNotDirectory, smb.StatusNotADirectory},
		{"directory", smb.ErrIsDirectory, smb.StatusFileIsADirectory},
		{"not-empty", smb.ErrDirectoryNotEmpty, smb.StatusDirectoryNotEmpty},
		{"full", smb.ErrDiskFull, smb.StatusDiskFull},
		{"too-large", smb.ErrFileTooLarge, smb.StatusFileTooLarge},
		{"unsupported", smb.ErrNotSupported, smb.StatusNotSupported},
		{"identity", smb.ErrIdentityChanged, smb.StatusObjectNameNotFound},
		{"io", smb.ErrIO, smb.StatusIODeviceError},
		{"resources", smb.ErrResources, smb.StatusInsufficientResources},
		{"cancel", context.Canceled, smb.StatusCancelled},
		{"deadline", context.DeadlineExceeded, smb.StatusIOTimeout},
		{"eof", io.EOF, smb.StatusEndOfFile},
		{"short-input", io.ErrUnexpectedEOF, smb.StatusInternalError},
		{"unknown", errors.New("backend detail"), smb.StatusInternalError},
		{"unknown-kind", smb.ErrorKind("future category"), smb.StatusInternalError},
		{"cancel-precedence", errors.Join(smb.ErrIO, context.Canceled), smb.StatusCancelled},
		{"deadline-precedence", errors.Join(smb.ErrIO, context.DeadlineExceeded), smb.StatusIOTimeout},
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
