package smbfs

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestReadRetrySuccess(t *testing.T) {
	calls := 0
	n, err := retryRead(t.Context(), func() (int, syscall.Errno) {
		calls++
		if calls <= 2 {
			return 0, syscall.EIO
		}
		return 7, 0
	}, readRetryWindow)
	if err != nil || n != 7 || calls != 3 {
		t.Fatalf("read = %d, %v after %d calls", n, err, calls)
	}
}

func TestReadRetryOtherErrors(t *testing.T) {
	for _, eno := range []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.EINVAL} {
		t.Run(eno.Error(), func(t *testing.T) {
			calls := 0
			n, err := retryRead(t.Context(), func() (int, syscall.Errno) {
				calls++
				return 2, eno
			}, readRetryWindow)
			if n != 2 || !errors.Is(err, eno) || calls != 1 {
				t.Fatalf("read = %d, %v after %d calls", n, err, calls)
			}
		})
	}
}

func TestReadRetryEagain(t *testing.T) {
	calls := 0
	n, err := retryRead(t.Context(), func() (int, syscall.Errno) {
		calls++
		if calls == 1 {
			return 0, syscall.EAGAIN
		}
		return 3, 0
	}, readRetryWindow)
	if n != 3 || err != nil || calls != 2 {
		t.Fatalf("read = %d, %v after %d calls", n, err, calls)
	}
}

func TestReadRetryDeadlineStartsAtFirstFailure(t *testing.T) {
	window := 50 * time.Millisecond
	var failed time.Time
	calls := 0
	n, err := retryRead(t.Context(), func() (int, syscall.Errno) {
		calls++
		// The initial backend attempt takes longer than our outage window.
		time.Sleep(2 * window)
		failed = time.Now()
		return 0, syscall.EIO
	}, window)
	if n != 0 || !errors.Is(err, syscall.EIO) || calls != 1 {
		t.Fatalf("read = %d, %v after %d calls", n, err, calls)
	}
	requireError(t, err, smb.ErrIO)
	if time.Since(failed) < window {
		t.Fatal("retry window started before the first failure")
	}
}

func TestReadRetryDeadlineDoesNotRestart(t *testing.T) {
	window := 2*readRetrySleep + readRetrySleep/2
	start := time.Now()
	calls := 0
	_, err := retryRead(t.Context(), func() (int, syscall.Errno) {
		calls++
		if time.Since(start) > 3*window {
			return 1, 0 // Bound the test if a failure restarts the deadline.
		}
		return 0, syscall.EIO
	}, window)
	if !errors.Is(err, syscall.EIO) || calls < 2 {
		t.Fatalf("read = %v after %d calls", err, calls)
	}
	if time.Since(start) < window {
		t.Fatal("returned before the retry deadline")
	}
}

func TestReadRetryCancellation(t *testing.T) {
	for _, when := range []string{"before read", "during wait", "context deadline"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			want := error(context.Canceled)
			if when == "before read" {
				cancel()
			}
			if when == "context deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				t.Cleanup(stop)
				want = context.DeadlineExceeded
			}
			called := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				_, err := retryRead(ctx, func() (int, syscall.Errno) {
					called <- struct{}{}
					return 0, syscall.EIO
				}, readRetryWindow)
				done <- err
			}()
			if when == "during wait" {
				<-called
				// Let the retry timer start before canceling the request.
				time.Sleep(10 * time.Millisecond)
				cancel()
			}
			select {
			case err := <-done:
				requireError(t, err, want)
			case <-time.After(time.Second):
				t.Fatal("cancellation did not stop retry wait")
			}
			if when == "before read" && len(called) != 0 {
				t.Fatal("canceled read called the backend")
			}
		})
	}
}
