// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// slowGetAttr answers GetAttr after the 300 ms root timeout, with an error.
type slowGetAttr struct{ engine }

func (slowGetAttr) doGetAttr(Context, Ino, *Attr) syscall.Errno {
	time.Sleep(400 * time.Millisecond)
	return syscall.EIO
}

// GetAttr on the root falls back to default attributes after 300 ms. Under
// -race this fails while the late lookup writes the result GetAttr already
// used.
func TestRootGetAttrAfterTimeout(t *testing.T) {
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
	db, ok := m.(*dbMeta)
	if !ok {
		t.Fatalf("NewSQLite returned %T", m)
	}
	db.en = slowGetAttr{db.en}
	var attr Attr
	if eno := m.GetAttr(Background(), RootInode, &attr); eno != 0 || attr.Typ != TypeDirectory {
		t.Fatalf("root GetAttr after the timeout: %v, type %d", eno, attr.Typ)
	}
	// Let the late lookup finish without synchronizing with it.
	time.Sleep(200 * time.Millisecond)
}
