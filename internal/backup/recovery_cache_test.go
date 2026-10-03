// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestRecoveryCacheKillBoundary(t *testing.T) {
	for _, boundary := range []string{"before-wipe", "after-wipe"} {
		t.Run(boundary, func(t *testing.T) {
			m, format := newMetadata(t)
			blob := newStore(t)
			mgr := newManager(t, m, blob, t.TempDir(), time.Now, time.Second)
			receipt, err := mgr.Backup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			encoded, err := json.Marshal(format)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "format.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecoveryCacheKillHelper$")
			cmd.Env = append(os.Environ(), "S3_SMB_KILL_ROOT="+root, "S3_SMB_KILL_BLOB="+blob.dir, "S3_SMB_KILL_KEY="+receipt.Key, "S3_SMB_KILL_BOUNDARY="+boundary)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					cancel()
					if err := cmd.Wait(); err != nil {
						t.Logf("recovery helper: %v\n%s", err, output.String())
					}
				}
			}()
			ready := filepath.Join(root, "ready")
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("recovery helper did not reach the kill boundary")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if err == nil {
				t.Fatal("killed recovery helper exited successfully")
			}
			state, cache := filepath.Join(root, "state"), filepath.Join(root, "cache")
			db := filepath.Join(state, "metadata.db")
			if _, err := os.Lstat(db); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("database published before cache wipe and rename: %v", err)
			}
			_, err = os.Stat(filepath.Join(cache, format.UUID))
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
			conf := meta.DefaultConf()
			conf.NoBGJob = true
			restored, err := storage.OpenMetadata(db, conf)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := restored.Shutdown(); err != nil {
					t.Error(err)
				}
			}()
			cacheFileRoundTrip(t, restored, blob, format, cache, "new", []byte("NEWNEW"))
		})
	}
}

// TestRecoveryCacheKillHelper runs in a child so killing recovery also stops
// every JuiceFS cache worker from the discarded writer.
func TestRecoveryCacheKillHelper(t *testing.T) {
	root := os.Getenv("S3_SMB_KILL_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	encoded, err := os.ReadFile(filepath.Join(root, "format.json"))
	if err != nil {
		t.Fatal(err)
	}
	var format meta.Format
	if err := json.Unmarshal(encoded, &format); err != nil {
		t.Fatal(err)
	}
	blob, err := object.CreateStorage("file", os.Getenv("S3_SMB_KILL_BLOB"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	state, cache := filepath.Join(root, "state"), filepath.Join(root, "cache")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(state, "metadata.db")
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	m, err := storage.OpenMetadata(db, conf)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Init(&format, false); err != nil {
		t.Fatal(err)
	}
	cacheFileRoundTrip(t, m, blob, &format, cache, "old", []byte("OLDOLD"))
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = recoverMetadata(context.Background(), blob, os.Getenv("S3_SMB_KILL_KEY"), db, &format, func() error {
		if os.Getenv("S3_SMB_KILL_BOUNDARY") == "after-wipe" {
			if err := WipeVolumeCache(cache, format.UUID, state); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(root, "ready"), []byte("ready"), 0600); err != nil {
			return err
		}
		// The parent kills this process while the restored database is staged.
		for {
			time.Sleep(time.Hour)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("recovery helper passed the kill boundary")
}

func hasCachedBlock(t *testing.T, root string) bool {
	t.Helper()
	found := false
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.Contains(path, string(os.PathSeparator)+"raw"+string(os.PathSeparator)) {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func cacheFileRoundTrip(t *testing.T, m meta.Meta, blob object.ObjectStorage, format *meta.Format, cache, name string, data []byte) {
	t.Helper()
	if _, err := m.Load(true); err != nil {
		t.Fatal(err)
	}
	capacity := int64(8 << 20)
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
	file, errno := runtime.FS.Create(ctx, "/"+name, 0600, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if n, errno := file.Write(ctx, data); errno != 0 || n != len(data) {
		t.Fatalf("write: %d/%d, %v", n, len(data), errno)
	}
	if errno := file.Fsync(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if errno := file.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
	file, errno = runtime.FS.Open(ctx, "/"+name, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	got := make([]byte, len(data))
	n, readErr := file.Read(ctx, got)
	if errno := file.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if readErr != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("read: got %q, want %q: %v", got, data, readErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !hasCachedBlock(t, cache) {
		if time.Now().After(deadline) {
			t.Fatal("file never reached the disk cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecoveryValidationFailureKeepsCache(t *testing.T) {
	m, format := newMetadata(t)
	blob := newStore(t)
	mgr := newManager(t, m, blob, t.TempDir(), time.Now, time.Second)
	receipt, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root, state := t.TempDir(), t.TempDir()
	volume := filepath.Join(root, format.UUID)
	if err := os.Mkdir(volume, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(volume, "keep")
	if err := os.WriteFile(keep, []byte("old cache"), 0600); err != nil {
		t.Fatal(err)
	}
	current := *format
	current.Name = "wrong-volume"
	db := filepath.Join(state, "metadata.db")
	if _, err := Recover(context.Background(), blob, receipt.Key, db, root, &current); err == nil {
		t.Fatal("accepted mismatched metadata")
	}
	got, err := os.ReadFile(keep)
	if err != nil || string(got) != "old cache" {
		t.Fatalf("cache changed before metadata validation: %q, %v", got, err)
	}
	if _, err := os.Lstat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database exists after validation failure: %v", err)
	}
}

func TestRecoveryCacheWipeFailureDoesNotPublish(t *testing.T) {
	m, format := newMetadata(t)
	blob := newStore(t)
	mgr := newManager(t, m, blob, t.TempDir(), time.Now, time.Second)
	receipt, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.WriteFile(cache, []byte("unrelated file"), 0600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "metadata.db")
	if _, err := Recover(context.Background(), blob, receipt.Key, db, cache, format); err == nil {
		t.Fatal("published a database after a failed cache wipe")
	}
	if _, err := os.Lstat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database exists after wipe failure: %v", err)
	}
	got, err := os.ReadFile(cache)
	if err != nil || string(got) != "unrelated file" {
		t.Fatalf("cache root file changed: %q, %v", got, err)
	}
}
