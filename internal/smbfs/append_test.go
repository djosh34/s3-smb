package smbfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
)

type appendMutationGate struct {
	entered, resume chan struct{}
	once            sync.Once
}

func (gate *appendMutationGate) pause() {
	gate.once.Do(func() { close(gate.entered) })
	<-gate.resume
}

type pausedAppendWriter struct {
	vfs.FileWriter
	gate *appendMutationGate
}

func (writer *pausedAppendWriter) Write(ctx meta.Context, offset uint64, data []byte) syscall.Errno {
	errno := writer.FileWriter.Write(ctx, offset, data)
	if errno == 0 {
		writer.gate.pause()
	}
	return errno
}

type pausedAppendMetadata struct {
	meta.Meta
	gate *appendMutationGate
	key  smb.ObjectKey
}

func (metadata *pausedAppendMetadata) Truncate(ctx meta.Context, ino meta.Ino, flags uint8, size uint64, attr *meta.Attr, skipPermCheck bool) syscall.Errno {
	errno := metadata.Meta.Truncate(ctx, ino, flags, size, attr, skipPermCheck)
	if errno == 0 && smb.Inode(ino) == metadata.key.Inode {
		metadata.gate.pause()
	}
	return errno
}

func (metadata *pausedAppendMetadata) SetXattr(ctx meta.Context, ino meta.Ino, name string, value []byte, flags uint32) syscall.Errno {
	errno := metadata.Meta.SetXattr(ctx, ino, name, value, flags)
	if errno == 0 && smb.Inode(ino) == metadata.key.Inode && name == metadata.key.Stream {
		metadata.gate.pause()
	}
	return errno
}

func awaitAppendMutation(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("inode operation did not complete")
		return context.DeadlineExceeded
	}
}

func TestAppendWriteCoordinatesWithMutations(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mutation := range []string{"write", "append", "truncate", "set size"} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, mutation), func(t *testing.T) {
				checkAppendMutation(t, stream, mutation)
			})
		}
	}
}

func checkAppendMutation(t *testing.T, stream bool, mutation string) {
	t.Helper()
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	baseHandle := f.open(t, base.Object, smb.AccessRead|smb.AccessWrite)
	selected := base
	if stream {
		write(t, f.fs, baseHandle, "base payload", 0)
		selected = f.create(t, "data:fork", smb.KindFile)
	}
	seed := f.open(t, selected.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, seed, "seed", 0)
	access := smb.AccessRead | smb.AccessWrite
	if mutation == "append" {
		access = smb.AccessRead | smb.AccessAppend
	}
	winner := f.open(t, selected.Object, access)
	appender := f.open(t, selected.Object, smb.AccessRead|smb.AccessAppend)
	other := f.create(t, "other", smb.KindFile)
	otherHandle := f.open(t, other.Object, smb.AccessRead|smb.AccessWrite)

	// Hold the real mutation inside the adapter coordinator, before it publishes
	// length. The second handle must validate EOF after that mutation completes.
	gate := &appendMutationGate{entered: make(chan struct{}), resume: make(chan struct{})}
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(gate.resume) }) })
	if stream || mutation == "truncate" || mutation == "set size" {
		f.fs.metadata = &pausedAppendMetadata{Meta: f.fs.metadata, gate: gate, key: selected.Object}
	} else {
		st, release := f.fs.acquire(selected.Object.Inode)
		st.writer = &pausedAppendWriter{FileWriter: st.writer, gate: gate}
		release()
	}
	first := make(chan error, 1)
	go func() {
		switch mutation {
		case "truncate":
			first <- f.fs.Truncate(t.Context(), winner, 10)
		case "set size":
			size := uint64(10)
			first <- f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{Size: &size})
		default:
			n, err := f.fs.WriteAt(t.Context(), winner, []byte("winner"), 4)
			if err == nil && n != 6 {
				err = fmt.Errorf("winner wrote %d bytes, want 6", n)
			}
			first <- err
		}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not reach the backend")
	}
	second := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		n, err := f.fs.WriteAt(t.Context(), appender, []byte("stale"), 4)
		if n != 0 {
			second <- fmt.Errorf("stale append wrote %d bytes, want 0", n)
			return
		}
		if err == nil {
			second <- errors.New("stale append succeeded")
			return
		}
		if !errors.Is(err, smb.ErrAccessDenied) {
			second <- fmt.Errorf("stale append: %w; want ACCESS_DENIED", err)
			return
		}
		second <- nil
	}()
	<-started
	progress := make(chan error, 1)
	go func() {
		n, err := f.fs.WriteAt(t.Context(), otherHandle, []byte("other"), 0)
		if err == nil && n != 5 {
			err = fmt.Errorf("unrelated write = %d, want 5", n)
		}
		progress <- err
	}()
	if err := awaitAppendMutation(t, progress); err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(gate.resume) })
	if err := awaitAppendMutation(t, first); err != nil {
		t.Fatal(err)
	}
	if err := awaitAppendMutation(t, second); err != nil {
		t.Fatal(err)
	}
	want := "seedwinner"
	if mutation == "truncate" || mutation == "set size" {
		want = "seed\x00\x00\x00\x00\x00\x00"
	}
	read(t, f.fs, seed, []byte(want))
	read(t, f.fs, otherHandle, []byte("other"))
	if stream {
		read(t, f.fs, baseHandle, []byte("base payload"))
	}
}

func TestAppendAccessAndSelectedEOF(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	fork := f.create(t, "data:fork", smb.KindFile)
	other := f.create(t, "data:other", smb.KindFile)
	baseHandle := f.open(t, base.Object, smb.AccessRead|smb.AccessWrite)
	forkHandle := f.open(t, fork.Object, smb.AccessRead|smb.AccessWrite)
	otherHandle := f.open(t, other.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, baseHandle, "base payload", 0)
	write(t, f.fs, forkHandle, "fork", 0)
	write(t, f.fs, otherHandle, "other stream", 0)
	for _, selected := range []smb.Resolved{base, fork, other} {
		appender := f.open(t, selected.Object, smb.AccessAppend)
		_, err := f.fs.ReadAt(t.Context(), appender, make([]byte, 1), 0)
		requireError(t, err, smb.ErrAccessDenied)
		for _, data := range [][]byte{nil, []byte("overwrite")} {
			n, writeErr := f.fs.WriteAt(t.Context(), appender, data, 0)
			if n != 0 {
				t.Fatalf("denied write = %d", n)
			}
			requireError(t, writeErr, smb.ErrAccessDenied)
		}
		requireError(t, f.fs.Truncate(t.Context(), appender, 0), smb.ErrAccessDenied)
		attr, err := f.fs.GetAttr(t.Context(), selected.Object)
		if err != nil {
			t.Fatal(err)
		}
		write(t, f.fs, appender, "!", attr.Size)
	}
	read(t, f.fs, baseHandle, []byte("base payload!"))
	read(t, f.fs, forkHandle, []byte("fork!"))
	read(t, f.fs, otherHandle, []byte("other stream!"))

	// Combined flags let CREATE initialize length without removing append checks.
	combined := f.open(t, fork.Object, smb.AccessWrite|smb.AccessAppend)
	if err := f.fs.Truncate(t.Context(), combined, 2); err != nil {
		t.Fatal(err)
	}
	_, err := f.fs.WriteAt(t.Context(), combined, []byte("bad"), 0)
	requireError(t, err, smb.ErrAccessDenied)
	write(t, f.fs, combined, "", 2)
	write(t, f.fs, combined, "!", 4)
	write(t, f.fs, forkHandle, "", maxStreamSize+1)
	write(t, f.fs, forkHandle, "OK", 0)
	read(t, f.fs, forkHandle, []byte("OK\x00\x00!"))
	read(t, f.fs, baseHandle, []byte("base payload!"))
	read(t, f.fs, otherHandle, []byte("other stream!"))
}
