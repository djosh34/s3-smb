// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/storage"
)

// The tests in this file run part of themselves in a child process, so the
// parent can kill it at a chosen point. The child runs in the parent's work
// directory and finds its inputs there.
const childEnv = "S3_SMB_BACKUP_CHILD"

func isChild() bool { return os.Getenv(childEnv) != "" }

// killChild runs test in a child process inside dir and kills it once it
// signals that it reached the point under test.
func killChild(t *testing.T, test, dir string, env ...string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^"+test+"$", "-test.timeout=1m")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(env, childEnv+"=1")...)
	cmd.ExtraFiles = []*os.File{w}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The read ends with EOF if the child exits without signalling.
	closeErr := w.Close()
	_, err = r.Read(make([]byte, 1))
	err = errors.Join(err, closeErr, r.Close())
	killErr := cmd.Process.Kill()
	if waitErr := cmd.Wait(); waitErr == nil {
		t.Fatalf("child exited by itself:\n%s", output.String())
	}
	if err != nil {
		t.Fatalf("child did not reach the kill point: %v\n%s", err, output.String())
	}
	if killErr != nil {
		t.Fatal(killErr)
	}
}

// signalParent tells the parent to kill this process now.
func signalParent() error {
	f := os.NewFile(3, "parent")
	_, err := f.Write([]byte{1})
	return errors.Join(err, f.Close())
}

// backupInto takes a backup of a fresh volume into dir/blob and records its key
// in dir for the child.
func backupInto(t *testing.T, dir string) (*fixtureStore, Receipt, *meta.Format) {
	t.Helper()
	m, format := newMetadata(t)
	s := openStore(t, filepath.Join(dir, "blob"))
	r := backupOnce(t, m, s)
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte(r.Key), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, r, format
}

// childInputs finds what backupInto recorded in the working directory.
func childInputs(t *testing.T) (dir string, s object.ObjectStorage, key string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	k, err := os.ReadFile("key")
	if err != nil {
		t.Fatal(err)
	}
	return dir, openStore(t, filepath.Join(dir, "blob")), string(k)
}

// A kill before or after the cache wipe leaves no database, so the next start
// recovers again. The new database then reads its own bytes for a reused
// slice ID, not the bytes the lost writer cached.
func TestRecoveryKilledAroundCacheWipe(t *testing.T) {
	if isChild() {
		recoverUntilKilled(t, os.Getenv("S3_SMB_KILL_BOUNDARY") == "after-wipe")
		return
	}
	for _, boundary := range []string{"before-wipe", "after-wipe"} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			blob, receipt, format := backupInto(t, dir)
			killChild(t, "TestRecoveryKilledAroundCacheWipe", dir, "S3_SMB_KILL_BOUNDARY="+boundary)
			state, cache := filepath.Join(dir, "state"), filepath.Join(dir, "cache")
			db := filepath.Join(state, "metadata.db")
			if _, err := os.Lstat(db); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("database published before the cache wipe and rename: %v", err)
			}
			_, err := os.Stat(filepath.Join(cache, format.UUID))
			if boundary == "before-wipe" && err != nil {
				t.Fatalf("cache wiped before the boundary: %v", err)
			}
			if boundary == "after-wipe" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cache still present after the wipe: %v", err)
			}
			if err := CleanupRecoveryStaging(state); err != nil {
				t.Fatal(err)
			}
			if _, err := Recover(context.Background(), blob, receipt.Key, db, cache, format); err != nil {
				t.Fatal(err)
			}
			cacheFileRoundTrip(t, openMetadata(t, db, false, 0), blob, format, cache, "new", []byte("NEWNEW"))
		})
	}
}

// recoverUntilKilled runs in the child. It writes a file through the disk cache
// with a fresh database, as the lost writer, then recovers the older backup.
func recoverUntilKilled(t *testing.T, wipe bool) {
	dir, blob, key := childInputs(t)
	format, err := Inspect(t.Context(), blob, key, dir)
	if err != nil {
		t.Fatal(err)
	}
	state, cache := filepath.Join(dir, "state"), filepath.Join(dir, "cache")
	if err = os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(state, "metadata.db")
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	m, err := storage.OpenMetadata(db, conf)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Init(format, false); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Load(true); err != nil {
		t.Fatal(err)
	}
	cacheFileRoundTrip(t, m, blob, format, cache, "old", []byte("OLDOLD"))
	if err = errors.Join(m.Shutdown(), os.RemoveAll(state)); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = recoverMetadata(context.Background(), blob, key, db, format, func() error {
		if wipe {
			if wipeErr := WipeVolumeCache(cache, format.UUID, state); wipeErr != nil {
				return wipeErr
			}
		}
		if signalErr := signalParent(); signalErr != nil {
			return signalErr
		}
		select {}
	})
	t.Fatal("recovery passed the kill point:", err)
}

// cacheFileRoundTrip writes and reads a file through the disk cache and waits
// until the block is in the cache.
func cacheFileRoundTrip(t *testing.T, m meta.Meta, blob object.ObjectStorage, format *meta.Format, cache, name string, data []byte) {
	t.Helper()
	capacity := uint64(8 << 20)
	runtime, err := storage.OpenFilesystem(m, blob, format, cache, &capacity, func() error { return ErrUnprotected })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := m.NewSession(false); err != nil {
		t.Fatal(err)
	}
	ctx := meta.Background()
	file, errno := runtime.FS.Create(ctx, "/"+name, 0o600, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	n, errno := file.Write(ctx, data)
	if errno != 0 || n != len(data) {
		t.Fatalf("write: %d/%d, %v", n, len(data), errno)
	}
	if errno = file.Fsync(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if errno = file.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if file, errno = runtime.FS.Open(ctx, "/"+name, 0); errno != 0 {
		t.Fatal(errno)
	}
	got := make([]byte, len(data))
	n, readErr := file.Read(ctx, got)
	if errno = file.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if readErr != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("read: got %q, want %q: %v", got, data, readErr)
	}
	// JuiceFS writes the disk cache in the background and has no completion
	// signal, so poll for the block.
	deadline := time.Now().Add(10 * time.Second)
	for !hasCachedBlock(t, cache) {
		if time.Now().After(deadline) {
			t.Fatal("file never reached the disk cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasCachedBlock(t *testing.T, root string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.Contains(path, string(os.PathSeparator)+"raw"+string(os.PathSeparator)) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// blockingStore stops a download after gzip has read its header, when the
// decrypted database file already exists.
type blockingStore struct{ object.ObjectStorage }

func (s blockingStore) Get(ctx context.Context, key string, off, limit int64, attrs ...object.AttrGetter) (io.ReadCloser, error) {
	r, err := s.ObjectStorage.Get(ctx, key, off, limit, attrs...)
	if err != nil {
		return nil, err
	}
	return &blockingReader{ReadCloser: r}, nil
}

type blockingReader struct {
	io.ReadCloser
	read bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return r.ReadCloser.Read(p[:min(len(p), 128)])
	}
	if err := signalParent(); err != nil {
		return 0, err
	}
	select {}
}

// An inspection killed mid-download leaves its decrypted copy only inside the
// state directory, where CleanupRecoveryStaging removes it.
func TestKilledInspectionIsCleanedUp(t *testing.T) {
	if isChild() {
		dir, blob, key := childInputs(t)
		_, err := Inspect(context.Background(), blockingStore{blob}, key, filepath.Join(dir, "state"))
		t.Fatal("inspection passed the kill point:", err)
	}
	dir := t.TempDir()
	backupInto(t, dir)
	state := filepath.Join(dir, "state")
	lock := filepath.Join(state, "state.lock")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	killChild(t, "TestKilledInspectionIsCleanedUp", dir)
	staged, err := filepath.Glob(filepath.Join(state, ".s3-smb-recovery-*", "metadata.db"))
	if err != nil || len(staged) != 1 {
		t.Fatalf("inspection staging in the state directory: %v %v", staged, err)
	}
	if err = CleanupRecoveryStaging(state); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(staged[0])); !os.IsNotExist(err) {
		t.Fatal("inspection staging was not cleaned:", err)
	}
	if data, err := os.ReadFile(filepath.Clean(lock)); err != nil || string(data) != "keep" {
		t.Fatal("cleanup changed a state file:", err)
	}
}
