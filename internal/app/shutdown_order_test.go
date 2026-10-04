// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
)

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

func (gate shutdownGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not reach cleanup gate")
	}
}

type gatedShutdownServer struct {
	smbServer
	gate shutdownGate
}

func (s gatedShutdownServer) Shutdown(ctx context.Context) error {
	if _, deadline := ctx.Deadline(); deadline {
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
	closed := make(chan error, 1)
	go func() { closed <- r.close() }()
	for _, gate := range []shutdownGate{serverGate, adapterGate} {
		gate.await(t)
		if err := backupCtx.Err(); err != nil {
			t.Fatalf("backup canceled before cleanup: %v", err)
		}
		select {
		case err := <-closed:
			t.Fatalf("close returned while cleanup was blocked: %v", err)
		default:
		}
		close(gate.release)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not finish")
	}
	if !server.called || !adapter.called || backupCtx.Err() == nil {
		t.Fatal("shutdown did not finish all cleanup")
	}
}

func TestShutdownReportsBackupFailure(t *testing.T) {
	failure := backup.ErrUnprotected
	done := make(chan error, 1)
	done <- failure
	close(done)
	if err := (&resources{backupDone: done}).close(); !errors.Is(err, failure) {
		t.Fatalf("lost backup failure: %v", err)
	}
}
