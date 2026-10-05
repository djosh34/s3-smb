// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/backup"
)

type shutdownServer struct {
	err    error
	called bool
}

func (*shutdownServer) Serve(context.Context, net.Listener) error { return nil }

func (s *shutdownServer) Shutdown(context.Context) error {
	s.called = true
	return s.err
}

type shutdownAdapter struct{ called bool }

func (a *shutdownAdapter) Shutdown() error {
	a.called = true
	return nil
}

// closeWithServeError runs close after Serve or Shutdown returned err. It
// reports the close error and whether storage cleanup ran and the state lock
// was released.
func closeWithServeError(t *testing.T, err error, fromShutdown bool) (cleanedUp bool, closeErr error) {
	t.Helper()
	s := &shutdownServer{}
	done := make(chan error, 1)
	if fromShutdown {
		s.err = err
	} else {
		done <- err
	}
	close(done)
	dir := t.TempDir()
	lock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &shutdownAdapter{}
	closeErr = (&resources{server: s, adapter: a, serveDone: done, lock: lock}).close(t.Context())
	other, err := lockState(dir)
	if err != nil {
		// The failed close kept the lock. Release it for the next case.
		if e := lock.Close(); e != nil {
			t.Fatal(e)
		}
		return false, closeErr
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.called || !a.called {
		t.Fatal("close released the state lock without finishing SMB cleanup")
	}
	return true, closeErr
}

func TestSMBShutdownSeparatesSignalsFromFailures(t *testing.T) {
	failure := errors.New("open close failed")
	wrappedClose := &net.OpError{Op: "close", Net: "tcp", Err: net.ErrClosed}
	canceledAccept := fmt.Errorf("accept failed: %w", context.Canceled)
	for _, test := range []struct {
		err  error
		want error
	}{
		{nil, nil},
		{context.Canceled, nil},
		{wrappedClose, nil},
		{fmt.Errorf("shutdown: %w", errors.Join(context.Canceled, wrappedClose)), nil},
		{canceledAccept, canceledAccept},
		{errors.Join(fmt.Errorf("close listener: %w", net.ErrClosed), failure), failure},
		{fmt.Errorf("shutdown: %w", errors.Join(context.Canceled, fmt.Errorf("close open: %w", errors.Join(net.ErrClosed, failure)))), failure},
		{&net.OpError{Op: "close", Net: "tcp", Err: errors.Join(net.ErrClosed, failure)}, failure},
	} {
		for _, fromShutdown := range []bool{false, true} {
			cleanedUp, err := closeWithServeError(t, test.err, fromShutdown)
			if test.want == nil {
				if err != nil || !cleanedUp {
					t.Fatalf("%v (from Shutdown %t): close %v, cleaned up %t", test.err, fromShutdown, err, cleanedUp)
				}
				continue
			}
			if !errors.Is(err, test.want) || cleanedUp {
				t.Fatalf("%v (from Shutdown %t): close %v, cleaned up %t", test.err, fromShutdown, err, cleanedUp)
			}
			if errors.Is(err, net.ErrClosed) || errors.Is(test.want, failure) && errors.Is(err, context.Canceled) {
				t.Fatalf("kept a shutdown signal: %v", err)
			}
		}
	}
}

type shutdownGate struct {
	entered chan struct{}
	release chan struct{}
}

func newShutdownGate(t *testing.T) shutdownGate {
	t.Helper()
	gate := shutdownGate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	return gate
}

func (gate shutdownGate) wait() {
	close(gate.entered)
	<-gate.release
}

type gatedShutdownServer struct {
	smbServer
	gate shutdownGate
}

func (s gatedShutdownServer) Shutdown(ctx context.Context) error {
	if _, deadline := ctx.Deadline(); deadline || ctx.Err() != nil {
		return errors.New("cleanup may outlive the app shutdown context")
	}
	s.gate.wait()
	return s.smbServer.Shutdown(ctx)
}

type gatedShutdownAdapter struct {
	smbAdapter
	gate shutdownGate
}

func (a gatedShutdownAdapter) Shutdown() error {
	a.gate.wait()
	return a.smbAdapter.Shutdown()
}

func TestShutdownWaitsBeforeStoppingBackup(t *testing.T) {
	serverGate, adapterGate := newShutdownGate(t), newShutdownGate(t)
	server := &shutdownServer{}
	adapter := &shutdownAdapter{}
	backupCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backupDone := make(chan error, 1)
	go func() {
		<-backupCtx.Done()
		backupDone <- backupCtx.Err()
		close(backupDone)
	}()
	r := resources{
		server: gatedShutdownServer{server, serverGate}, adapter: gatedShutdownAdapter{adapter, adapterGate},
		cancelBackup: cancel, backupDone: backupDone,
	}
	// close gets an ended context, as it does in serve.
	ended, end := context.WithCancel(t.Context())
	end()
	closed := make(chan error, 1)
	go func() { closed <- r.close(ended) }()
	for _, gate := range []shutdownGate{serverGate, adapterGate} {
		select {
		case <-gate.entered:
		case err := <-closed:
			t.Fatalf("close returned before cleanup: %v", err)
		}
		if err := backupCtx.Err(); err != nil {
			t.Fatalf("backup canceled before cleanup: %v", err)
		}
		close(gate.release)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if !server.called || !adapter.called || backupCtx.Err() == nil {
		t.Fatal("shutdown did not finish all cleanup")
	}
}

func backupResult(err error) <-chan error {
	done := make(chan error, 1)
	done <- err
	close(done)
	return done
}

func TestShutdownReportsBackupFailure(t *testing.T) {
	failure := backup.ErrUnprotected
	if err := (&resources{backupDone: backupResult(failure)}).close(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("lost backup failure: %v", err)
	}
}

// A backup canceled during its retry wait returns the cancellation joined with
// the last failed attempt. That is a normal shutdown.
func TestShutdownAcceptsCanceledBackupRetry(t *testing.T) {
	retry := errors.Join(context.Canceled, fmt.Errorf("upload snapshot: %w", context.DeadlineExceeded))
	if err := (&resources{backupDone: backupResult(retry)}).close(t.Context()); err != nil {
		t.Fatalf("canceled backup retry failed shutdown: %v", err)
	}
}
