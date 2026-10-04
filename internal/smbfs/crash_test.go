package smbfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// The child stops after a successful cross-handle flush. The parent kills it
// without Close or session cleanup, then reopens the same database and objects.
func TestCrossHandleFlushSurvivesProcessKill(t *testing.T) {
	if dir := os.Getenv("SMBFS_CRASH_DIR"); dir != "" {
		mode, err := strconv.ParseUint(os.Getenv("SMBFS_CRASH_MODE"), 10, 8)
		if err != nil {
			t.Fatal(err)
		}
		crashWriter(t, dir, smb.SyncMode(mode))
		return
	}
	for _, mode := range []smb.SyncMode{smb.SyncData, smb.SyncFull} {
		t.Run(strconv.FormatUint(uint64(mode), 10), func(t *testing.T) {
			dir := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestCrossHandleFlushSurvivesProcessKill$", "-test.timeout=30s") //nolint:gosec // The child is this test executable, returned by os.Executable.
			command.Env = append(os.Environ(), "SMBFS_CRASH_DIR="+dir, "SMBFS_CRASH_MODE="+strconv.FormatUint(uint64(mode), 10))
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			waitForCrashMarker(ctx, t, command, filepath.Join(dir, "flushed"))
			if err = command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) {
				t.Fatalf("killed child = %v; output: %s", err, output.String())
			}
			f := fixtureAt(t, dir, 0, false)
			for _, entry := range []struct {
				path string
				data string
			}{{"data", "durable payload"}, {"truncated", "abnew"}, {"data:fork", "stream"}} {
				r, lookupErr := f.fs.Lookup(t.Context(), entry.path)
				if lookupErr != nil || !r.Exists {
					t.Fatalf("reopened %q = %+v, %v", entry.path, r, lookupErr)
				}
				h := f.open(t, r.Object, smb.AccessRead)
				read(t, f.fs, h, []byte(entry.data))
			}
			r, err := f.fs.Lookup(t.Context(), "truncated")
			stamp := time.Unix(1000000000, 123456700)
			if err != nil || !r.Attr.Modified.Equal(stamp) {
				t.Fatalf("reopened timestamp = %v, %v", r.Attr.Modified, err)
			}
		})
	}
}

func crashWriter(t *testing.T, dir string, mode smb.SyncMode) {
	t.Helper()
	f := fixtureAt(t, dir, 0, true)
	r := f.create(t, "data", smb.KindFile)
	a := f.open(t, r.Object, smb.AccessWrite)
	b := f.open(t, r.Object, smb.AccessRead)
	write(t, f.fs, a, "durable payload", 0)
	if err := f.fs.Flush(t.Context(), b, mode); err != nil {
		t.Fatal(err)
	}
	stream := f.create(t, "data:fork", smb.KindFile)
	h := f.open(t, stream.Object, smb.AccessWrite)
	write(t, f.fs, h, "stream", 0)
	if err := f.fs.Flush(t.Context(), h, mode); err != nil {
		t.Fatal(err)
	}
	truncated := f.create(t, "truncated", smb.KindFile)
	x := f.open(t, truncated.Object, smb.AccessRead|smb.AccessWrite)
	y := f.open(t, truncated.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, x, "abcdefgh", 0)
	if err := f.fs.Truncate(t.Context(), y, 2); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, x, "new", 2)
	stamp := time.Unix(1000000000, 123456700)
	if err := f.fs.SetAttr(t.Context(), truncated.Object, smb.AttrChange{Modified: &stamp}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Flush(t.Context(), y, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(filepath.Join(dir, "flushed")), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-t.Context().Done()
}

func waitForCrashMarker(ctx context.Context, t *testing.T, command *exec.Cmd, path string) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			if err := command.Wait(); err != nil {
				t.Logf("child: %v", err)
			}
			t.Fatal("child did not reach flush marker")
		}
	}
}
