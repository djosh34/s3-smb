package smbserver

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type unusedFS struct{ vfs.VFSFileSystem }

func TestCompositionRequiresShareAndNamedAccount(t *testing.T) {
	for _, tc := range []struct{ share, account string }{{"", "backup"}, {"backup", ""}, {"IPC$", "backup"}, {"bad/share", "backup"}} {
		if _, err := New(tc.share, tc.account, "", unusedFS{}); err == nil {
			t.Fatal("invalid account/share accepted")
		}
	}
	if _, err := New("backup", "backup", "", nil); err == nil {
		t.Fatal("nil filesystem accepted")
	}
	for _, password := range []string{"fixture-password", ""} {
		s, err := New("backup", "backup", password, unusedFS{})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
