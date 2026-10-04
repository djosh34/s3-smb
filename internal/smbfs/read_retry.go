package smbfs

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

const (
	readRetryWindow = smb.S3OutageWindow + time.Minute
	readRetrySleep  = 100 * time.Millisecond
)

// retryRead gives each read its own outage window, starting at the call.
// An in-flight backend attempt may finish after the window, so a permanent
// failure takes at most the window plus one backend retry budget.
func retryRead(ctx context.Context, attempt func() (int, syscall.Errno), window time.Duration) (int, error) {
	deadline := time.Now().Add(window)
	var n int
	var eno syscall.Errno
	for {
		if err := ctx.Err(); err != nil {
			return n, errors.Join(backendError(eno), err)
		}
		if !time.Now().Before(deadline) {
			return n, backendError(syscall.EIO)
		}
		n, eno = attempt()
		if eno != syscall.EIO && eno != syscall.EAGAIN || ctx.Err() != nil {
			return n, errors.Join(backendError(eno), ctx.Err())
		}
		if eno == syscall.EAGAIN {
			continue
		}
		timer := time.NewTimer(min(readRetrySleep, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return n, errors.Join(backendError(eno), ctx.Err())
		case <-timer.C:
		}
	}
}
