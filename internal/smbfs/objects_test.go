package smbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestIssue123SelectedStreamUsesFileCalls(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	baseHandle := f.open(t, base.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, baseHandle, "base bytes", 0)
	stream := f.create(t, "data:com.apple.ResourceFork:$DATA", smb.KindFile)
	other := f.create(t, "data:AFP_AfpInfo", smb.KindFile)
	h := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	otherHandle := f.open(t, other.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, otherHandle, "finder", 0)
	write(t, f.fs, h, "abc", 4)
	write(t, f.fs, h, "Z", 5)
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a', 'Z', 'c'})
	size := uint64(5)
	if err := f.fs.SetAttr(t.Context(), h.Key(), smb.AttrChange{Size: &size}); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a'})
	if err := f.fs.Truncate(t.Context(), h, 8); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a', 0, 0, 0})
	write(t, f.fs, h, "!", maxStreamSize-1)
	_, err := f.fs.WriteAt(t.Context(), h, []byte("!"), maxStreamSize)
	requireError(t, err, smb.ErrFileTooLarge)
	requireError(t, f.fs.Truncate(t.Context(), h, maxStreamSize+1), smb.ErrFileTooLarge)
	attr, err := f.fs.GetAttr(t.Context(), h.Key())
	if err != nil || attr.Size != maxStreamSize {
		t.Fatalf("stream attr = %+v, %v", attr, err)
	}
	streams, err := f.fs.Streams(t.Context(), base.Object.Inode)
	if err != nil || len(streams) != 2 {
		t.Fatalf("streams = %+v, %v", streams, err)
	}
	if err = f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Remove(t.Context(), stream.Name, base.Object.Inode); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, baseHandle, []byte("base bytes"))
	read(t, f.fs, otherHandle, []byte("finder"))
	_, err = f.fs.GetAttr(t.Context(), h.Key())
	requireError(t, err, smb.ErrNameNotFound)
}

func TestNamedStreamRenameIsNotSupported(t *testing.T) {
	f := newFixture(t, 0)
	f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	h := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "stream", 0)
	destination := stream.Name
	destination.Stream = "other"
	requireError(t, f.fs.Rename(t.Context(), smb.RenameRequest{Source: stream.Name, Destination: destination, SourceInode: stream.Object.Inode}), smb.ErrNotSupported)
	read(t, f.fs, h, []byte("stream"))
	missing, err := f.fs.Lookup(t.Context(), "data:other")
	if err != nil || missing.Exists {
		t.Fatalf("rename created destination: %+v, %v", missing, err)
	}
	destination.Stream = ""
	requireError(t, f.fs.Rename(t.Context(), smb.RenameRequest{Source: destination, Destination: stream.Name, SourceInode: stream.Object.Inode}), smb.ErrNotSupported)
}

func TestStoragePrimitivesForCreateDispositions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, disposition := range []string{"open", "create", "open_if", "overwrite", "overwrite_if", "supersede"} {
			t.Run(disposition+"/"+map[bool]string{false: "file", true: "stream"}[stream], func(t *testing.T) { testDisposition(t, stream, disposition) })
		}
		for _, disposition := range []string{"open_if", "overwrite_if"} {
			t.Run(disposition+"/missing/"+map[bool]string{false: "file", true: "stream"}[stream], func(t *testing.T) {
				f := newFixture(t, 0)
				path := "data"
				if stream {
					f.create(t, path, smb.KindFile)
					path += ":fork"
				}
				missing, err := f.fs.Lookup(t.Context(), path)
				if err != nil || missing.Exists {
					t.Fatalf("missing lookup = %+v, %v", missing, err)
				}
				created, err := f.fs.Create(t.Context(), missing.Name, smb.KindFile)
				if err != nil {
					t.Fatal(err)
				}
				h := f.open(t, created.Object, smb.AccessRead|smb.AccessWrite)
				read(t, f.fs, h, []byte{})
				write(t, f.fs, h, "created", 0)
				read(t, f.fs, h, []byte("created"))
			})
		}
	}
}

func testDisposition(t *testing.T, stream bool, disposition string) {
	t.Helper()
	f := newFixture(t, 0)
	p := "data"
	if stream {
		f.create(t, p, smb.KindFile)
		p += ":fork"
	}
	r := f.create(t, p, smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "original", 0)
	lookup, err := f.fs.Lookup(t.Context(), p)
	if err != nil || !lookup.Exists || lookup.Object != r.Object {
		t.Fatalf("lookup = %+v, %v", lookup, err)
	}
	switch disposition {
	case "open", "open_if":
		read(t, f.fs, h, []byte("original"))
	case "create":
		_, err = f.fs.Create(t.Context(), r.Name, smb.KindFile)
		requireError(t, err, smb.ErrNameCollision)
	case "overwrite", "overwrite_if":
		if err = f.fs.Truncate(t.Context(), h, 0); err != nil {
			t.Fatal(err)
		}
		read(t, f.fs, h, []byte{})
	case "supersede":
		if err = f.fs.Close(t.Context(), h); err != nil {
			t.Fatal(err)
		}
		if err = f.fs.Remove(t.Context(), r.Name, r.Object.Inode); err != nil {
			t.Fatal(err)
		}
		replacement := f.create(t, p, smb.KindFile)
		fresh := f.open(t, replacement.Object, smb.AccessRead)
		read(t, f.fs, fresh, []byte{})
		if !stream && replacement.Object.Inode == r.Object.Inode {
			t.Fatal("supersede reused base identity")
		}
	}
}

func TestNamespaceChangesRequireExpectedIdentities(t *testing.T) {
	for _, trashDays := range []int{0, 14} {
		t.Run(strconv.Itoa(trashDays), func(t *testing.T) { testNamespaceChanges(t, trashDays) })
	}
}

func testNamespaceChanges(t *testing.T, trashDays int) {
	t.Helper()
	f := fixtureAt(t, t.TempDir(), 0, true, trashDays)
	source := f.create(t, "source", smb.KindFile)
	destination := f.create(t, "destination", smb.KindFile)
	a := f.open(t, source.Object, smb.AccessRead|smb.AccessWrite)
	b := f.open(t, destination.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, a, "source", 0)
	write(t, f.fs, b, "destination", 0)
	requireError(t, f.fs.Remove(t.Context(), source.Name, destination.Object.Inode), smb.ErrIdentityChanged)
	request := smb.RenameRequest{Source: source.Name, Destination: destination.Name, SourceInode: source.Object.Inode, DestinationInode: 0, Replace: true}
	requireError(t, f.fs.Rename(t.Context(), request), smb.ErrIdentityChanged)
	request.DestinationInode = destination.Object.Inode
	request.SourceInode = destination.Object.Inode
	requireError(t, f.fs.Rename(t.Context(), request), smb.ErrIdentityChanged)
	read(t, f.fs, a, []byte("source"))
	read(t, f.fs, b, []byte("destination"))
	request.SourceInode = source.Object.Inode
	if err := f.fs.Rename(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	path, err := f.fs.PathOf(t.Context(), source.Object.Inode)
	if err != nil || path != "destination" {
		t.Fatalf("path = %q, %v", path, err)
	}
	if _, err = f.fs.PathOf(t.Context(), destination.Object.Inode); !errors.Is(err, smb.ErrNameNotFound) {
		t.Fatalf("replaced inode path = %v", err)
	}
	read(t, f.fs, a, []byte("source"))
	read(t, f.fs, b, []byte("destination"))
	if err = f.fs.Remove(t.Context(), destination.Name, source.Object.Inode); err != nil {
		t.Fatal(err)
	}
	_, err = f.fs.PathOf(t.Context(), source.Object.Inode)
	requireError(t, err, smb.ErrNameNotFound)
	read(t, f.fs, a, []byte("source"))
}

func TestReadOnlyRejectsAllMutations(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	ro, err := New(Options{Filesystem: f.native, Barrier: f.fs.barrier, ReadOnly: true, MetadataPath: f.path, Config: f.config, Store: f.chunks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := ro.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	for _, r := range []smb.Resolved{base, stream} {
		for _, access := range []smb.Access{smb.AccessWrite, smb.AccessAppend, smb.AccessWrite | smb.AccessAppend} {
			_, err = ro.Open(t.Context(), r.Object, access)
			requireError(t, err, smb.ErrReadOnly)
		}
		h, openErr := ro.Open(t.Context(), r.Object, smb.AccessRead)
		if openErr != nil {
			t.Fatal(openErr)
		}
		_, err = ro.WriteAt(t.Context(), h, []byte("x"), 0)
		requireError(t, err, smb.ErrReadOnly)
		requireError(t, ro.Truncate(t.Context(), h, 0), smb.ErrReadOnly)
		size := uint64(0)
		requireError(t, ro.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}), smb.ErrReadOnly)
		requireError(t, ro.Remove(t.Context(), r.Name, r.Object.Inode), smb.ErrReadOnly)
		if err = ro.Close(t.Context(), h); err != nil {
			t.Fatal(err)
		}
	}
	_, err = ro.Create(t.Context(), smb.Name{Parent: 1, Base: "new"}, smb.KindFile)
	requireError(t, err, smb.ErrReadOnly)
	requireError(t, ro.Rename(t.Context(), smb.RenameRequest{Source: base.Name, Destination: smb.Name{Parent: 1, Base: "new"}, SourceInode: base.Object.Inode}), smb.ErrReadOnly)
}

func TestPathsRootAndDirectoryCookies(t *testing.T) {
	f := newFixture(t, 0)
	root, err := f.fs.Lookup(t.Context(), "")
	if err != nil || !root.Exists || root.Object.Inode != 1 || root.Attr.Kind != smb.KindDirectory {
		t.Fatalf("root = %+v, %v", root, err)
	}
	dir := f.create(t, "dir", smb.KindDirectory)
	f.create(t, "dir/a", smb.KindFile)
	f.create(t, "dir/b", smb.KindFile)
	nested, err := f.fs.Lookup(t.Context(), "dir\\b::$DATA")
	if err != nil || !nested.Exists || nested.Object.Stream != "" {
		t.Fatalf("nested = %+v, %v", nested, err)
	}
	for _, p := range []string{"../data", "dir/../data", "/data", "dir//a", "dir/.", ".trash/x", ".stats", "dir/a\x00", "dir/a:bad:other", "dir/a:"} {
		if _, err = f.fs.Lookup(t.Context(), p); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	_, err = f.fs.Lookup(t.Context(), "absent/leaf")
	requireError(t, err, smb.ErrPathNotFound)
	missing, err := f.fs.Lookup(t.Context(), "dir/missing")
	if err != nil || missing.Exists || missing.Name.Parent != dir.Object.Inode {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
	entries, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, 0, 1)
	if err != nil || len(entries) != 1 || entries[0].Name != "a" {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	entries, err = f.fs.ReadDir(t.Context(), dir.Object.Inode, entries[0].Next, 1)
	if err != nil || len(entries) != 1 || entries[0].Name != "b" {
		t.Fatalf("continuation = %+v, %v", entries, err)
	}
	entries, err = f.fs.ReadDir(t.Context(), dir.Object.Inode, entries[0].Next, 1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("exhaustion = %+v, %v", entries, err)
	}
	if _, err = f.fs.Open(t.Context(), smb.ObjectKey{}, smb.AccessRead); !errors.Is(err, smb.ErrInvalidParameter) {
		t.Fatalf("zero inode = %v", err)
	}
}

func TestStatFSConfiguredCapacityAndDefaultCap(t *testing.T) {
	for _, capacity := range []uint64{0, 500000000000, 2000000000000} {
		f := newFixture(t, capacity)
		space, err := f.fs.StatFS(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if space.Available > space.Free || space.Free > space.Capacity || space.VolumeID == 0 {
			t.Fatalf("space = %+v", space)
		}
		if capacity != 0 && space.Capacity != capacity {
			t.Fatalf("capacity = %d; want %d", space.Capacity, capacity)
		}
		if capacity == 0 && space.Free > 1<<40 {
			t.Fatalf("unlimited free = %d", space.Free)
		}
		if capacity > 1<<40 && space.Free <= 1<<40 {
			t.Fatal("configured capacity was capped")
		}
	}
}

func TestAttributesPersistAndStayOutOfStreams(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	stamp := time.Unix(-123456, 123456789).UTC()
	attributes := uint32(0x22)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Created: &stamp, Accessed: &stamp, Modified: &stamp, Changed: &stamp, Attributes: &attributes}); err != nil {
		t.Fatal(err)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Created.Equal(stamp) || !a.Accessed.Equal(stamp) || !a.Modified.Equal(stamp) || !a.Changed.Equal(stamp) || a.Attributes != attributes {
		t.Fatalf("attr = %+v, %v", a, err)
	}
	streams, err := f.fs.Streams(t.Context(), r.Object.Inode)
	if err != nil || len(streams) != 0 {
		t.Fatalf("private attrs listed as streams: %+v, %v", streams, err)
	}
}

func TestLaterWritesUpdateExplicitTimesButKeepCreationTime(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	initial := r.Attr.Created
	h := f.open(t, r.Object, smb.AccessWrite)
	stamp := time.Unix(1000000000, 123456700)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Changed: &stamp}); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "data", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.After(stamp) || !a.Changed.After(stamp) || !a.Created.Equal(initial) {
		t.Fatalf("later attrs = %+v, %v", a, err)
	}
}

func TestBaseOffsetsAndZeroFilledExtensions(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	other := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "abc", 4)
	read(t, f.fs, other, []byte{0, 0, 0, 0, 'a', 'b', 'c'})
	if err := f.fs.Truncate(t.Context(), other, 10); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a', 'b', 'c', 0, 0, 0})
}

func TestIssue124ReferencesHaveNoOpenOrLockPolicy(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	a := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	b := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	// Overlapping writes are storage operations, not SMB share or lock checks.
	write(t, f.fs, a, "first", 0)
	write(t, f.fs, b, "other", 0)
	if err := f.fs.Close(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	requireError(t, f.fs.Close(t.Context(), a), smb.ErrInvalidHandle)
	read(t, f.fs, b, []byte("other"))
	if err := f.fs.Close(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if len(f.fs.inodes) != 0 || len(f.fs.parents) != 0 {
		t.Fatal("unused coherence state retained")
	}
	// The persisted SQLite plock table remains empty.
	session, err := f.metadata.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range session {
		if len(entry.Flocks) != 0 || len(entry.Plocks) != 0 {
			t.Fatal("storage references created native locks")
		}
	}
}

func TestConcurrentInodeWritesAndQueries(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	a := f.open(t, r.Object, smb.AccessWrite)
	b := f.open(t, r.Object, smb.AccessWrite)
	var group sync.WaitGroup
	errs := make(chan error, 3)
	for index, h := range []smb.Handle{a, b} {
		group.Go(func() {
			var offset uint64
			if index == 1 {
				offset = 100
			}
			for i := uint64(0); i < 10; i++ {
				_, err := f.fs.WriteAt(t.Context(), h, bytes.Repeat([]byte("ab")[index:index+1], 10), offset+i*10)
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	group.Go(func() {
		for i := 0; i < 20; i++ {
			_, err := f.fs.GetAttr(t.Context(), r.Object)
			if err != nil {
				errs <- err
				return
			}
		}
	})
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	attr, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || attr.Size != 200 {
		t.Fatalf("size = %d, %v", attr.Size, err)
	}
}

func TestAccessCancellationAndInvalidRanges(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	readOnly := f.open(t, r.Object, smb.AccessRead)
	_, err := f.fs.WriteAt(t.Context(), readOnly, []byte("x"), 0)
	requireError(t, err, smb.ErrAccessDenied)
	writer := f.open(t, r.Object, smb.AccessWrite)
	_, err = f.fs.ReadAt(t.Context(), writer, make([]byte, 1), 0)
	requireError(t, err, smb.ErrAccessDenied)
	_, err = f.fs.WriteAt(t.Context(), writer, []byte("x"), math.MaxUint64)
	requireError(t, err, smb.ErrFileTooLarge)
	_, err = f.fs.ReadAt(t.Context(), readOnly, make([]byte, 1), math.MaxUint64)
	requireError(t, err, io.EOF)
	requireError(t, f.fs.Truncate(t.Context(), writer, math.MaxUint64), smb.ErrFileTooLarge)
	requireError(t, f.fs.Flush(t.Context(), writer, smb.SyncMode(9)), smb.ErrInvalidParameter)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.fs.Lookup(ctx, "data")
	requireError(t, err, context.Canceled)
	_, err = f.fs.WriteAt(ctx, writer, []byte("x"), 0)
	requireError(t, err, context.Canceled)
	_, err = f.fs.Lookup(t.Context(), "data:"+strings.Repeat("x", 256))
	requireError(t, err, smb.ErrInvalidName)
}
