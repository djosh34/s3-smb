// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

func serverResources(t *testing.T) (*resources, string) {
	t.Helper()
	return serverResourcesWithMetadata(t, func(m meta.Meta) meta.Meta { return m })
}

func serverResourcesWithMetadata(t *testing.T, wrap func(meta.Meta) meta.Meta) (*resources, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.db")
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	m, err := storage.OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	m = wrap(m)
	r := &resources{metadata: m}
	t.Cleanup(func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	})
	format, err := storage.NewFormat("app-test", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	format.Capacity = 2 << 40
	if err = m.Init(format, false); err != nil {
		t.Fatal(err)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o770}
	if eno := m.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	blob, err := object.CreateStorage("file", t.TempDir()+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	r.runtime, err = storage.OpenFilesystem(m, blob, format, "/unusable", &zero, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r.session = true
	if err = m.NewSession(true); err != nil {
		t.Fatal(err)
	}
	return r, path
}

func serverConfig() config.SMBConfig {
	return config.SMBConfig{Listen: "127.0.0.1:0", Share: "Backups", Username: "backup", Password: "password", Encryption: true}
}

func TestSMBListenerFailureHasNoFallback(t *testing.T) {
	r, path := serverResources(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	c := serverConfig()
	c.Listen = listener.Addr().String()
	if err = r.startSMB(t.Context(), c, path); err == nil {
		t.Fatal("started on an occupied address")
	}
	if r.listener != nil || r.serveDone != nil {
		t.Fatal("failed listener started serving")
	}
	if r.server == nil || r.adapter == nil {
		t.Fatal("constructor resources were lost on listener failure")
	}
}

type shutdownServer struct {
	called bool
	err    error
}

func (s *shutdownServer) Serve(context.Context, net.Listener) error { return nil }
func (s *shutdownServer) Shutdown(context.Context) error {
	s.called = true
	return s.err
}

type shutdownAdapter struct{ called bool }

func (a *shutdownAdapter) Shutdown() error {
	a.called = true
	return nil
}

func TestSMBShutdownDoesNotHideJoinedServeError(t *testing.T) {
	acceptErr := errors.New("accept failed")
	for _, signal := range []error{context.Canceled, net.ErrClosed} {
		t.Run(signal.Error(), func(t *testing.T) {
			done := make(chan error, 1)
			done <- errors.Join(signal, acceptErr)
			close(done)
			r := &resources{serveDone: done}
			if err := r.close(); !errors.Is(err, acceptErr) {
				t.Fatalf("lost joined accept error: %v", err)
			}
		})
	}
}

func TestSMBShutdownAcceptsOnlyExpectedServeErrors(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, net.ErrClosed, errors.Join(context.Canceled, net.ErrClosed)} {
		done := make(chan error, 1)
		done <- err
		close(done)
		r := &resources{serveDone: done}
		if err := r.close(); err != nil {
			t.Fatal(err)
		}
	}
	// A wrapped error carries context beyond the shutdown signal.
	wrapped := fmt.Errorf("accept failed: %w", context.Canceled)
	done := make(chan error, 1)
	done <- wrapped
	close(done)
	if err := (&resources{serveDone: done}).close(); !errors.Is(err, wrapped) {
		t.Fatalf("lost wrapped accept error: %v", err)
	}
}

func TestSMBShutdownFailureKeepsStorageAndStateLock(t *testing.T) {
	failure := errors.New("open close failed")
	s := &shutdownServer{err: failure}
	a := &shutdownAdapter{}
	dir := t.TempDir()
	lock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	r := &resources{server: s, adapter: a, lock: lock}
	if err = r.close(); !errors.Is(err, failure) {
		t.Fatalf("shutdown error: %v", err)
	}
	if !s.called || a.called {
		t.Fatal("storage closed after failed server shutdown")
	}
	other, err := lockState(dir)
	if err == nil {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("state lock released after failed server shutdown")
	}
}
