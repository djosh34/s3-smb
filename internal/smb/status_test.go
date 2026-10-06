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
		{smb.ErrIdentityChanged, "identity", smb.StatusObjectNameNotFound},
		{errors.Join(smb.ErrIO, errors.New("backend detail")), "joined-backend", smb.StatusIODeviceError},
		{errors.Join(smb.ErrIO, io.EOF), "classified-eof", smb.StatusIODeviceError},
		{io.EOF, "eof", smb.StatusEndOfFile},
		{errors.New("backend detail"), "unknown", smb.StatusInternalError},
		{smb.ErrorKind("future category"), "unknown-kind", smb.StatusInternalError},
		{errors.Join(smb.ErrIO, context.Canceled), "cancel-precedence", smb.StatusCancelled},
		{errors.Join(smb.ErrIO, context.DeadlineExceeded), "deadline-precedence", smb.StatusIODeviceError},
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
