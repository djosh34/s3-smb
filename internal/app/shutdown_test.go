// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
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

// shutdownStore is an engine that only shuts down.
type shutdownStore struct {
	smb.Storage
	err    error
	called bool
}

func (*shutdownStore) Dead() <-chan struct{} { return nil }

func (*shutdownStore) Err() error { return nil }

func (s *shutdownStore) Shutdown(context.Context) error {
	s.called = true
	return s.err
}

// closeWithServeError runs close after Serve or Shutdown returned err. It
// reports the close error and whether the engine was shut down and the folder
// lock released.
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
	a := &shutdownStore{}
	closeErr = (&resources{server: s, store: a, serveDone: done, lock: lock}).close(t.Context())
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
		t.Fatal("close released the folder lock without finishing SMB and engine cleanup")
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

// SMB shuts down, closing every open, before the engine stops.
func TestShutdownStopsSMBBeforeTheEngine(t *testing.T) {
	gate := newShutdownGate(t)
	server := &shutdownServer{}
	engine := &shutdownStore{}
	r := resources{server: gatedShutdownServer{server, gate}, store: engine}
	// close gets an ended context, as it does in serve.
	ended, end := context.WithCancel(t.Context())
	end()
	closed := make(chan error, 1)
	go func() { closed <- r.close(ended) }()
	select {
	case <-gate.entered:
	case err := <-closed:
		t.Fatalf("close returned before SMB shutdown: %v", err)
	}
	if engine.called {
		t.Fatal("engine stopped before SMB shutdown finished")
	}
	close(gate.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if !server.called || !engine.called {
		t.Fatal("shutdown did not finish all cleanup")
	}
}

// A failed engine shutdown is reported and keeps the folder lock.
func TestShutdownReportsEngineFailure(t *testing.T) {
	dir := t.TempDir()
	lock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := lock.Close(); e != nil {
			t.Error(e)
		}
	}()
	failure := errors.New("flush failed")
	if err = (&resources{store: &shutdownStore{err: failure}, lock: lock}).close(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("lost engine failure: %v", err)
	}
	if other, e := lockState(dir); e == nil {
		t.Fatal(errors.Join(errors.New("folder lock released after a failed shutdown"), other.Close()))
	}
}
