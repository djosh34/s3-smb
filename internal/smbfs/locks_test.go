package smbfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func requireNoNativeLocks(t *testing.T, f *fixture, inode smb.Inode) {
	t.Helper()
	// Query rows directly, including SID zero and crashed, nonzero sessions.
	for _, query := range []string{
		"SELECT COUNT(*) FROM jfs_plock WHERE inode = ?",
		"SELECT COUNT(*) FROM jfs_flock WHERE inode = ?",
	} {
		var count int
		if err := f.fs.directory.QueryRowContext(t.Context(), query, inode).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s: got %d native lock rows", query, count)
		}
	}
}

func TestIssue101AdapterIONeverPersistsNativeLocks(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	requireNoNativeLocks(t, f, base.Object.Inode)
	stream := f.create(t, "data:fork", smb.KindFile)
	requireNoNativeLocks(t, f, base.Object.Inode)
	for _, object := range []smb.ObjectKey{base.Object, stream.Object} {
		a := f.open(t, object, smb.AccessRead|smb.AccessWrite)
		requireNoNativeLocks(t, f, object.Inode)
		b := f.open(t, object, smb.AccessRead|smb.AccessWrite)
		requireNoNativeLocks(t, f, object.Inode)
		write(t, f.fs, a, "first", 0)
		requireNoNativeLocks(t, f, object.Inode)
		write(t, f.fs, b, "other", 0)
		requireNoNativeLocks(t, f, object.Inode)
		read(t, f.fs, a, []byte("other"))
		requireNoNativeLocks(t, f, object.Inode)
		for _, mode := range []smb.SyncMode{smb.SyncData, smb.SyncFull} {
			if err := f.fs.Flush(t.Context(), b, mode); err != nil {
				t.Fatal(err)
			}
			requireNoNativeLocks(t, f, object.Inode)
		}
		if err := f.fs.Truncate(t.Context(), a, 3); err != nil {
			t.Fatal(err)
		}
		requireNoNativeLocks(t, f, object.Inode)
		size := uint64(2)
		if err := f.fs.SetAttr(t.Context(), object, smb.AttrChange{Size: &size}); err != nil {
			t.Fatal(err)
		}
		requireNoNativeLocks(t, f, object.Inode)
		read(t, f.fs, b, []byte("ot"))
		requireNoNativeLocks(t, f, object.Inode)
		for _, h := range []smb.Handle{a, b} {
			if err := f.fs.Close(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			requireNoNativeLocks(t, f, object.Inode)
		}
	}
}

func lockTestOpen(t *testing.T, table *state.Table, h smb.Handle, session uint64) state.Open {
	t.Helper()
	reservation, status := table.Reserve(state.OpenRequest{
		Object: h.Key(), Binding: state.Binding{SessionID: session, TreeID: 1},
		GrantedAccess: 3, Sharing: 7,
	})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := table.Commit(reservation, state.Grant{Handle: h})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

func crashWithStateLocks(t *testing.T, dir string) {
	t.Helper()
	f := fixtureAt(t, dir, 0, true, 0)
	object := f.create(t, "data", smb.KindFile).Object
	h := f.open(t, object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "locked range", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	owner := lockTestOpen(t, table, h, 1)
	other := lockTestOpen(t, table, f.open(t, object, smb.AccessRead|smb.AccessWrite), 2)
	if status := table.Lock(owner.ID, owner.Binding, []state.Range{{Offset: 0, Length: 12, Exclusive: true}}, false); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	for _, write := range []bool{false, true} {
		if status := table.CheckIO(other.ID, other.Binding, 0, 12, write); status != smb.StatusFileLockConflict {
			t.Fatalf("held lock, write=%t: %v", write, status)
		}
	}
	requireNoNativeLocks(t, f, object.Inode)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(root.WriteFile("flushed", []byte("locked"), 0o600), root.Close()); err != nil {
		t.Fatal(err)
	}
	<-t.Context().Done()
}

func checkRestartedLockRange(t *testing.T, dir string, readOnly bool) {
	t.Helper()
	f := fixtureAtMode(t, dir, 0, false, 0, readOnly)
	r, err := f.fs.Lookup(t.Context(), "data")
	if err != nil || !r.Exists {
		t.Fatalf("reopened data = %+v, %v", r, err)
	}
	requireNoNativeLocks(t, f, r.Object.Inode)
	access := smb.AccessRead
	if !readOnly {
		access |= smb.AccessWrite
	}
	h := f.open(t, r.Object, access)
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	open := lockTestOpen(t, table, h, 3)
	for _, write := range []bool{false, true} {
		if status := table.CheckIO(open.ID, open.Binding, 0, 12, write); status != smb.StatusSuccess {
			t.Fatalf("restarted lock check, write=%t: %v", write, status)
		}
	}
	read(t, f.fs, h, []byte("locked range"))
	if readOnly {
		_, err = f.fs.WriteAt(t.Context(), h, []byte("new contents"), 0)
		requireError(t, err, smb.ErrReadOnly)
	} else {
		write(t, f.fs, h, "new contents", 0)
		read(t, f.fs, h, []byte("new contents"))
	}
	requireNoNativeLocks(t, f, r.Object.Inode)
}

func TestIssue101StateLocksDoNotSurviveProcessKill(t *testing.T) {
	if dir := os.Getenv("SMBFS_LOCK_CRASH_DIR"); dir != "" {
		crashWithStateLocks(t, dir)
		return
	}
	for _, mode := range []struct {
		name     string
		readOnly bool
	}{{"writable", false}, {"read-only", true}} {
		t.Run(mode.name, func(t *testing.T) {
			dir := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestIssue101StateLocksDoNotSurviveProcessKill$", "-test.timeout=30s") //nolint:gosec // The child is this test executable, returned by os.Executable.
			command.Env = append(os.Environ(), "SMBFS_LOCK_CRASH_DIR="+dir)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			waitForCrashMarker(ctx, t, command, filepath.Join(dir, "flushed"))
			if err = command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			var exitError *exec.ExitError
			if err = command.Wait(); !errors.As(err, &exitError) {
				t.Fatalf("killed child = %v; output: %s", err, output.String())
			}
			checkRestartedLockRange(t, dir, mode.readOnly)
		})
	}
}
