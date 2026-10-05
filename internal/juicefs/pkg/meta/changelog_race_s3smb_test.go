// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"path/filepath"
	"testing"
)

// The session refresh reloads the format while metadata writes check whether
// to write a change log entry. Under -race this fails without the getFormat
// call in genLog.
func TestLoadWhileWritingChangeLog(t *testing.T) {
	m, err := NewSQLite(filepath.Join(t.TempDir(), "metadata.db"), DefaultConf())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	if err := m.Init(&Format{Name: "test", UUID: "test", Storage: "file", BlockSize: 4096}, true); err != nil {
		t.Fatal(err)
	}
	loaded := make(chan error, 1)
	go func() {
		for range 20 {
			if _, err := m.Load(false); err != nil {
				loaded <- err
				return
			}
		}
		loaded <- nil
	}()
	for i := range 20 {
		if eno := m.SetXattr(Background(), RootInode, "user.test", []byte{byte(i)}, 0); eno != 0 {
			t.Fatal(eno)
		}
	}
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
}
