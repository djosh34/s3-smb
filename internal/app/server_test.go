// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

// serverResources holds an engine on an in-memory S3 server for SMB server
// tests. Cleanup closes SMB before the engine's own cleanup runs.
func serverResources(t *testing.T) *resources {
	t.Helper()
	storage := smbtest.NewStorage(t)
	r := &resources{}
	t.Cleanup(func() {
		if e := r.stopSMB(t.Context()); e != nil {
			t.Error(e)
		}
	})
	r.store = storage
	return r
}

func serverConfig() config.SMBConfig {
	return config.SMBConfig{Listen: "127.0.0.1:0", Share: "Backups", Username: "backup", Password: "password", Encryption: true}
}

func TestSMBListenerFailureHasNoFallback(t *testing.T) {
	r := serverResources(t)
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := listener.Close(); e != nil {
			t.Error(e)
		}
	}()
	c := serverConfig()
	c.Listen = listener.Addr().String()
	if err = r.startSMB(t.Context(), c); err == nil {
		t.Fatal("started on an occupied address")
	}
	if r.listener != nil || r.serveDone != nil {
		t.Fatal("failed listener started serving")
	}
	if r.server == nil {
		t.Fatal("constructor resources were lost on listener failure")
	}
}
