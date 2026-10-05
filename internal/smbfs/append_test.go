package smbfs

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"

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

// An append-only write checks EOF under the same inode lock as every other
// length change. Each case holds a mutation inside the backend and starts an
// append at the old EOF; once the mutation completes the append must fail.
func TestAppendWriteCoordinatesWithMutations(t *testing.T) {
	for _, c := range []struct {
		mutation string
		stream   bool
	}{
		{"write", false},
		{"set size", false},
		{"write", true},
		{"truncate", true},
	} {
		t.Run(fmt.Sprintf("stream_%t/%s", c.stream, c.mutation), func(t *testing.T) {
			checkAppendMutation(t, c.stream, c.mutation)
		})
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
	winner := f.open(t, selected.Object, smb.AccessRead|smb.AccessWrite)
	appender := f.open(t, selected.Object, smb.AccessRead|smb.AccessAppend)

	gate := &appendMutationGate{entered: make(chan struct{}), resume: make(chan struct{})}
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(gate.resume) }) })
	if stream || mutation != "write" {
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
			_, err := f.fs.WriteAt(t.Context(), winner, []byte("winner"), 4)
			first <- err
		}
	}()
	receive(t, gate.entered)
	second := make(chan error, 1)
	go func() {
		n, err := f.fs.WriteAt(t.Context(), appender, []byte("stale"), 4)
		if n != 0 {
			err = errors.Join(err, fmt.Errorf("stale append wrote %d bytes", n))
		}
		second <- err
	}()
	unblock.Do(func() { close(gate.resume) })
	if err := receive(t, first); err != nil {
		t.Fatal(err)
	}
	requireError(t, receive(t, second), smb.ErrAccessDenied)
	want := "seedwinner"
	if mutation != "write" {
		want = "seed\x00\x00\x00\x00\x00\x00"
	}
	read(t, f.fs, seed, []byte(want))
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
