package smbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// The child stops after successful flushes, including one through another
// handle. The parent kills it without Close or session cleanup, then reopens
// the same database and objects.
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
			runUntilFlushed(t, executable, "SMBFS_CRASH_DIR="+dir, "SMBFS_CRASH_MODE="+strconv.FormatUint(uint64(mode), 10))
			f := fixtureAt(t, dir, 0, false, 0)
			for _, entry := range []struct {
				path string
				data string
			}{{"data", "durable payload"}, {"truncated", "abnew"}, {"data:fork", "stream"}} {
				r, lookupErr := f.fs.Lookup(t.Context(), entry.path)
				if lookupErr != nil || !r.Exists {
					t.Fatalf("reopened %q = %+v, %v", entry.path, r, lookupErr)
				}
				read(t, f.fs, f.open(t, r.Object, smb.AccessRead), []byte(entry.data))
			}
			r, err := f.fs.Lookup(t.Context(), "truncated")
			stamp := time.Unix(1000000000, 123456700)
			if err != nil || !r.Attr.Modified.Equal(stamp) {
				t.Fatalf("reopened timestamp = %v, %v", r.Attr.Modified, err)
			}
		})
	}
}

// runUntilFlushed runs the crash test in executable with env added, waits until
// the child reports its flushes through a pipe, and kills it.
func runUntilFlushed(t *testing.T, executable string, env ...string) {
	t.Helper()
	ready, signal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestCrossHandleFlushSurvivesProcessKill$", "-test.timeout=30s")
	command.Env = append(os.Environ(), env...)
	command.ExtraFiles = []*os.File{signal}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err = command.Start()
	err = errors.Join(err, signal.Close())
	if err != nil {
		t.Fatal(errors.Join(err, ready.Close()))
	}
	// The read ends with EOF if the child exits or is killed at the deadline.
	_, readErr := ready.Read(make([]byte, 1))
	err = errors.Join(ready.Close(), command.Process.Kill())
	waitErr := command.Wait()
	if readErr != nil {
		t.Fatalf("child did not flush: %v, %v; output: %s", readErr, waitErr, output.String())
	}
	var exitError *exec.ExitError
	if err != nil || !errors.As(waitErr, &exitError) {
		t.Fatalf("kill child: %v, %v; output: %s", err, waitErr, output.String())
	}
}

func crashWriter(t *testing.T, dir string, mode smb.SyncMode) {
	t.Helper()
	f := fixtureAt(t, dir, 0, true, 0)
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
	signal := os.NewFile(3, "flushed")
	if _, err := io.WriteString(signal, "."); err != nil {
		t.Fatal(errors.Join(err, signal.Close()))
	}
	if err := signal.Close(); err != nil {
		t.Fatal(err)
	}
	<-t.Context().Done()
}
