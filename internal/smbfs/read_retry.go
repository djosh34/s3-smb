package smbfs

import (
	"context"
	"errors"
	"syscall"
	"time"
)

const (
	readRetryWindow = 6 * time.Minute
	readRetrySleep  = 100 * time.Millisecond
)

// retryRead gives each read its own outage window, starting at its first EIO.
// The backend may spend time retrying before returning that first failure.
func retryRead(ctx context.Context, attempt func() (int, syscall.Errno), window time.Duration) (int, error) {
	var deadline time.Time
	var n int
	var eno syscall.Errno
	for {
		if err := ctx.Err(); err != nil {
			return n, errors.Join(backendError(eno), err)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return n, backendError(syscall.EIO)
		}
		n, eno = attempt()
		if eno != syscall.EIO && eno != syscall.EAGAIN || ctx.Err() != nil {
			return n, errors.Join(backendError(eno), ctx.Err())
		}
		if eno == syscall.EAGAIN {
			continue
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(window)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return n, backendError(eno)
		}
		timer := time.NewTimer(min(readRetrySleep, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return n, errors.Join(backendError(eno), ctx.Err())
		case <-timer.C:
		}
	}
}
