// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type corruptReadbackStore struct{ *fixtureStore }

func (s corruptReadbackStore) Get(context.Context, string, int64, int64, ...object.AttrGetter) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("wrong stored bytes")), nil
}

func TestSnapshotReadbackFailureDoesNotOpenProtection(t *testing.T) {
	m, _ := newMetadata(t)
	s := corruptReadbackStore{newStore(t)}
	state := t.TempDir()
	mgr := newManager(t, m, s, state, time.Now, time.Minute)
	if _, err := mgr.Backup(context.Background()); err == nil || !strings.Contains(err.Error(), "readback mismatch") {
		t.Fatal("accepted corrupt readback", err)
	}
	if !errors.Is(mgr.opts.Protection.Check(), ErrUnprotected) {
		t.Fatal("readback failure left protection open")
	}
	if _, err := os.Stat(filepath.Join(state, "backup-receipt.json")); !os.IsNotExist(err) {
		t.Fatal("readback failure published a receipt", err)
	}
	entries, err := os.ReadDir(filepath.Join(state, "backup-staging"))
	if err != nil || len(entries) != 0 {
		t.Fatal("backup leaked staging after failure", entries, err)
	}
}

func TestSnapshotFailureNeverUploads(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	mgr := newManager(t, m, s, t.TempDir(), time.Now, time.Minute)
	mgr.opts.DatabasePath = filepath.Join(t.TempDir(), "missing.db")
	if _, err := mgr.Backup(context.Background()); err == nil {
		t.Fatal("accepted failed snapshot")
	}
	if s.puts.Load() != 0 {
		t.Fatal("uploaded incomplete snapshot")
	}
}

func TestSnapshotStagingCleanupLeavesUnknownFiles(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "snapshot-owned")
	unknown := filepath.Join(root, "snapshot-unknown")
	for _, dir := range []string{owned, unknown} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"metadata.db", "snapshot.db.gz"} {
		if err := os.WriteFile(filepath.Join(owned, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(unknown, "unrelated")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "snapshot-link")
	if err := os.Symlink(unknown, link); err != nil {
		t.Fatal(err)
	}
	if err := cleanupSnapshotStaging(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatal("left owned abandoned snapshot", err)
	}
	if data, err := os.ReadFile(keep); err != nil || !bytes.Equal(data, []byte("keep")) {
		t.Fatal("removed unknown staging file", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("removed unknown staging symlink", err)
	}
}
