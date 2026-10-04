// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type interruptedInspectionStore struct {
	object.ObjectStorage
	ready string
}

func (s interruptedInspectionStore) Get(ctx context.Context, key string, off, limit int64, attrs ...object.AttrGetter) (io.ReadCloser, error) {
	r, err := s.ObjectStorage.Get(ctx, key, off, limit, attrs...)
	if err != nil {
		return nil, err
	}
	return &interruptedInspectionReader{ReadCloser: r, ready: s.ready}, nil
}

type interruptedInspectionReader struct {
	io.ReadCloser
	ready string
	read  bool
}

func (r *interruptedInspectionReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		// Let gzip read its header. The next read occurs after downloadSnapshot
		// has created the decrypted SQLite file in the inspection directory.
		if len(p) > 128 {
			p = p[:128]
		}
		return r.ReadCloser.Read(p)
	}
	if err := os.WriteFile(r.ready, nil, 0600); err != nil {
		return 0, err
	}
	select {} // The parent kills this process during the download.
}

func TestInterruptedInspectionStaysInStateDirectoryAndIsCleaned(t *testing.T) {
	if state := os.Getenv("S3_SMB_INSPECTION_CHILD_STATE"); state != "" {
		raw, err := object.CreateStorage("file", os.Getenv("S3_SMB_INSPECTION_CHILD_REMOTE"), "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		blob := interruptedInspectionStore{raw, os.Getenv("S3_SMB_INSPECTION_CHILD_READY")}
		if _, err = Inspect(context.Background(), blob, os.Getenv("S3_SMB_INSPECTION_CHILD_KEY"), state); err != nil {
			t.Fatal(err)
		}
		t.Fatal("inspection should have waited for the parent to kill it")
	}
	m, _ := newMetadata(t)
	s := newStore(t)
	r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	lock := filepath.Join(state, "state.lock")
	if err = os.WriteFile(lock, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInterruptedInspectionStaysInStateDirectoryAndIsCleaned$", "-test.timeout=10s")
	cmd.Env = append(os.Environ(), "S3_SMB_INSPECTION_CHILD_STATE="+state, "S3_SMB_INSPECTION_CHILD_REMOTE="+s.dir, "S3_SMB_INSPECTION_CHILD_KEY="+r.Key, "S3_SMB_INSPECTION_CHILD_READY="+ready)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		if !joined {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			var exitErr *exec.ExitError
			if err := cmd.Wait(); err != nil && !errors.As(err, &exitErr) {
				t.Error(err)
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("inspection child did not reach the staged download")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	joined = true
	if err == nil {
		t.Fatal("inspection child was not killed")
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	var staging string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".s3-smb-recovery-") {
			staging = filepath.Join(state, e.Name())
		}
	}
	if staging == "" {
		t.Fatal("interrupted inspection left no staging under the state directory")
	}
	if _, err = os.Stat(filepath.Join(staging, "metadata.db")); err != nil {
		t.Fatal("inspection did not stage its decrypted database in state", err)
	}
	if err = CleanupRecoveryStaging(state); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("inspection leftover was not cleaned", err)
	}
	if data, err := os.ReadFile(lock); err != nil || string(data) != "keep" {
		t.Fatal("inspection cleanup changed a state file", err)
	}
}

func TestInspectionRequiresAbsoluteStateDirectory(t *testing.T) {
	for _, state := range []string{"", "relative"} {
		if _, err := Inspect(context.Background(), newStore(t), "meta/snapshot-2026-01-01-000000.db.gz", state); err == nil {
			t.Fatal("inspection accepted a missing or relative state directory")
		}
	}
}
