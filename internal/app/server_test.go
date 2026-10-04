// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

// serverResources opens a JuiceFS volume on local files for SMB server tests.
func serverResources(t *testing.T) (*resources, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.db")
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	m, err := storage.OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	r := &resources{metadata: m}
	t.Cleanup(func() {
		if e := r.close(t.Context()); e != nil {
			t.Error(e)
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
	if eno := m.SetAttr(meta.WrapContext(t.Context()), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	blob, err := object.CreateStorage("file", t.TempDir()+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
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
