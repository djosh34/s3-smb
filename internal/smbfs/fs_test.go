// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	jvfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type failingStore struct {
	object.ObjectStorage
	fail atomic.Bool
}

func (s *failingStore) Put(ctx context.Context, key string, r io.Reader, getters ...object.AttrGetter) error {
	if s.fail.Load() {
		return syscall.ENOSPC
	}
	return s.ObjectStorage.Put(ctx, key, r, getters...)
}

type fixture struct {
	s      *FS
	native *jfs.FileSystem
	m      meta.Meta
	store  *failingStore
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	disk, e := object.CreateStorage("file", filepath.Join(t.TempDir(), "objects")+"/", "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	return fixtureWithStore(t, &failingStore{ObjectStorage: disk}, nil)
}
func fixtureWithStore(t *testing.T, store *failingStore, dump []byte, readonly ...bool) *fixture {
	t.Helper()
	mc := meta.DefaultConf()
	mc.NoBGJob = true
	mc.MaxDeletes = 0
	m := meta.NewClient("sqlite3://"+filepath.Join(t.TempDir(), "meta.db"), mc)
	format := &meta.Format{Name: "adapter-test", UUID: "adapter-fixture", Storage: "file", BlockSize: 64, Compression: "none", Capacity: 1 << 30, TrashDays: 14, DirStats: true}
	if dump == nil {
		if e := m.Init(format, true); e != nil {
			t.Fatal(e)
		}
		a := meta.Attr{Uid: UID, Gid: GID, Mode: 0770}
		if er := m.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &a); er != 0 {
			t.Fatal(er)
		}
	} else {
		if e := m.LoadMeta(bytes.NewReader(dump)); e != nil {
			t.Fatal(e)
		}
	}
	readOnly := len(readonly) > 0 && readonly[0]
	mc.ReadOnly = readOnly // Set before session/filesystem startup, never toggle a live client.
	if !readOnly {
		if e := m.NewSession(true); e != nil {
			t.Fatal(e)
		}
	}
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 1, MaxDownload: 1, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: time.Second, PutTimeout: time.Second}
	chunks := chunk.NewCachedStore(store, cc, nil)
	native, e := jfs.NewFileSystem(&jvfs.Config{Meta: mc, Format: *format, Chunk: &cc}, m, chunks, nil)
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(native, readOnly)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		store.fail.Store(false)
		if e := s.Shutdown(); e != nil {
			t.Logf("shutdown (fault tests may retain failed writer): %v", e)
		}
		_ = native.Close()
		_ = m.CloseSession()
		_ = m.Shutdown()
	})
	return &fixture{s: s, native: native, m: m, store: store}
}
func openFile(t *testing.T, s *FS, p string) vfs.VfsHandle {
	t.Helper()
	h, e := s.Open(p, syscall.O_CREAT|syscall.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func requireErr(t *testing.T, e, want error) {
	t.Helper()
	if !errors.Is(e, want) {
		t.Fatalf("error = %v; want %v", e, want)
	}
}
func TestNativeIOAndAttributes(t *testing.T) {
	f := newFixture(t)
	s := f.s
	h := openFile(t, s, "data")
	if n, e := s.Write(h, []byte("abc"), 4, 1); e != nil || n != 3 {
		t.Fatalf("write = %d %v", n, e)
	}
	b := make([]byte, 10)
	n, e := s.Read(h, b, 0, 0)
	if e != nil || !bytes.Equal(b[:n], []byte{0, 0, 0, 0, 'a', 'b', 'c'}) {
		t.Fatalf("read=%q %v", b[:n], e)
	}
	a, e := s.GetAttr(h)
	if e != nil {
		t.Fatal(e)
	}
	size, _ := a.GetSizeBytes()
	uid, _ := a.GetUID()
	if size != 7 || uid != UID {
		t.Fatalf("size/uid=%d/%d", size, uid)
	}
	if e = s.Truncate(h, 2); e != nil {
		t.Fatal(e)
	}
	stamp := time.Unix(1700000000, 123456789)
	a, e = s.SetAttr(h, new(vfs.Attributes).SetLastDataModificationTime(stamp).SetUnixMode(0640))
	if e != nil {
		t.Fatal(e)
	}
	got, _ := a.GetLastDataModificationTime()
	mode, _ := a.GetUnixMode()
	if !got.Equal(stamp) || mode != 0640 {
		t.Fatalf("mtime/mode=%v/%o", got, mode)
	}
	key := "com.apple.ResourceFork"
	value := []byte("resource fork")
	if e = s.Setxattr(h, key, value); e != nil {
		t.Fatal(e)
	}
	n, e = s.Getxattr(h, key, nil)
	if e != nil || n != len(value) {
		t.Fatalf("xattr size=%d %v", n, e)
	}
	_, e = s.Getxattr(h, key, make([]byte, 1))
	requireErr(t, e, syscall.ERANGE)
	b = make([]byte, n)
	n, e = s.Getxattr(h, key, b)
	if e != nil || !bytes.Equal(b[:n], value) {
		t.Fatalf("xattr=%q %v", b[:n], e)
	}
	keys, e := s.Listxattr(h)
	if e != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("keys=%v %v", keys, e)
	}
	if e = s.Removexattr(h, key); e != nil {
		t.Fatal(e)
	}
	if e = s.Flush(h); e != nil {
		t.Fatal(e)
	}
	if e = s.FSync(h); e != nil {
		t.Fatal(e)
	}
	if _, e = s.StatFS(h); e != nil {
		t.Fatal(e)
	}
	_, e = s.Read(h, b, math.MaxUint64, 0)
	requireErr(t, e, syscall.EINVAL)
	_, e = s.Write(h, b, math.MaxInt64, 0)
	requireErr(t, e, syscall.EINVAL)
	if e = s.Close(h); e != nil {
		t.Fatal(e)
	}
	_, e = s.Read(h, b, 0, 0)
	requireErr(t, e, syscall.EBADF)
}
func TestDirectoryCursorRestart(t *testing.T) {
	s := newFixture(t).s
	if _, e := s.Mkdir("dir", 0700); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"dir/a", "dir/b"} {
		h := openFile(t, s, p)
		if e := s.Close(h); e != nil {
			t.Fatal(e)
		}
	}
	h, e := s.OpenDir("dir")
	if e != nil {
		t.Fatal(e)
	}
	first, e := s.ReadDir(h, vfs.ReadDirContinue, 1)
	if e != nil || len(first) != 1 {
		t.Fatalf("first=%v %v", first, e)
	}
	c := openFile(t, s, "dir/c")
	_ = s.Close(c)
	rest, e := s.ReadDir(h, vfs.ReadDirContinue, 10)
	if e != nil || len(rest) != 1 || rest[0].Name == first[0].Name {
		t.Fatalf("rest=%v %v", rest, e)
	}
	all, e := s.ReadDir(h, vfs.ReadDirRestart, 0)
	if e != nil || len(all) != 3 {
		t.Fatalf("restart=%v %v", all, e)
	}
	a, e := s.Lookup(h, "c")
	if e != nil || a.GetFileType() != vfs.FileTypeRegularFile {
		t.Fatalf("lookup=%v %v", a, e)
	}
	root, e := s.OpenDir("")
	if e != nil {
		t.Fatal(e)
	}
	all, e = s.ReadDir(root, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range all {
		if entry.Name == meta.TrashName {
			t.Fatal("trash exposed")
		}
	}
}
func TestLinksRenameAndStaleHandles(t *testing.T) {
	f := newFixture(t)
	s := f.s
	h := openFile(t, s, "file")
	if _, e := s.Write(h, []byte("contents"), 0, 1); e != nil {
		t.Fatal(e)
	}
	a, e := s.GetAttr(h)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Link(a.GetFileHandle(), vfs.VFS_ROOT_NODE, "hard"); e != nil {
		t.Fatal(e)
	}
	hard, e := s.Open("hard", syscall.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	ha, e := s.GetAttr(hard)
	if e != nil || ha.GetInodeNumber() != a.GetInodeNumber() {
		t.Fatalf("hardlink %v %v", ha, e)
	}
	sy := openFile(t, s, "sym")
	if _, e = s.Symlink(sy, "file", 1); e != nil {
		t.Fatal(e)
	}
	target, e := s.Readlink(sy)
	if e != nil || target != "file" {
		t.Fatalf("readlink=%q %v", target, e)
	}
	sh, e := s.Open("sym", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 8)
	if _, e = s.Read(sh, b, 0, 0); e != nil || string(b) != "contents" {
		t.Fatalf("symlink read %q %v", b, e)
	}
	if e = s.Rename(h, "renamed", 0); e != nil {
		t.Fatal(e)
	}
	if e = s.Unlink(h); e != nil {
		t.Fatal(e)
	}
	replacement := openFile(t, s, "renamed")
	_ = replacement
	requireErr(t, s.Unlink(h), syscall.EACCES)
	if _, e = s.Write(hard, []byte("live"), 0, 0); e != nil {
		t.Fatalf("live non-trash hardlink: %v", e)
	}
	dangling := openFile(t, s, "dangling")
	if _, e = s.Symlink(dangling, "missing/target", 1); e != nil {
		t.Fatalf("dangling internal link: %v", e)
	}
	// A renamed directory updates path-bearing child handles.
	if _, e = s.Mkdir("dir", 0700); e != nil {
		t.Fatal(e)
	}
	dh, e := s.OpenDir("dir")
	if e != nil {
		t.Fatal(e)
	}
	child := openFile(t, s, "dir/child")
	if e = s.Rename(dh, "moved", 0); e != nil {
		t.Fatal(e)
	}
	if e = s.Rename(child, "child-out", 0); e != nil {
		t.Fatal(e)
	}
}
func TestConfinementTrashAndStaleHandles(t *testing.T) {
	f := newFixture(t)
	s := f.s
	for _, p := range []string{"../outside", "a/../../outside", "/etc/passwd", "C:\\escape", ".trash", ".config", ".control", ".stats"} {
		if h, e := s.Open(p, syscall.O_RDWR|syscall.O_CREAT, 0600); e == nil {
			_ = s.Close(h)
			t.Fatalf("accepted %q", p)
		}
	}
	h := openFile(t, s, "link")
	for _, target := range []string{"../escape", "/absolute", ".trash", ".config"} {
		if _, e := s.Symlink(h, target, 1); e == nil {
			t.Fatalf("accepted target %q", target)
		}
	}
	// Simulate an existing alias in imported metadata, not just adapter creation.
	if er := f.native.Symlink(meta.Background(), ".trash", "/alias"); er != 0 {
		t.Fatal(er)
	}
	if _, e := s.Open("alias", syscall.O_RDONLY, 0); e == nil {
		t.Fatal("resolved trash alias accepted")
	}
	if _, e := s.Mkdir("alias/purge", 0700); e == nil {
		t.Fatal("resolved trash alias mutation accepted")
	}
	if er := f.native.Symlink(meta.Background(), ".config", "/config-alias"); er != 0 {
		t.Fatal(er)
	}
	if _, e := s.Open("config-alias", syscall.O_RDONLY, 0); e == nil {
		t.Fatal("resolved native internal node accepted")
	}
	if er := f.native.Mkdir(meta.Background(), "/private", 0700, 0); er != 0 {
		t.Fatal(er)
	}
	if _, e := s.OpenDir("private"); e == nil {
		t.Fatal("unprivileged client bypassed directory permission")
	}
	// Out-of-band namespace replacement must not retarget an existing handle.
	old := openFile(t, s, "old")
	if er := f.native.Rename(meta.Background(), "/old", "/saved", 0); er != 0 {
		t.Fatal(er)
	}
	replacement := openFile(t, s, "old")
	_ = replacement
	requireErr(t, s.Unlink(old), syscall.ESTALE)
	requireErr(t, s.Rename(old, "stolen", 1), syscall.ESTALE)
}
func TestTrashHandlePreservesExportedData(t *testing.T) {
	f := newFixture(t)
	s := f.s
	h := openFile(t, s, "protected")
	original := []byte("protected backup content")
	if _, e := s.Write(h, original, 0, 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Flush(h); e != nil {
		t.Fatal(e)
	}
	var dump bytes.Buffer
	if e := f.m.DumpMeta(&dump, meta.RootInode, 1, true, true, false); e != nil {
		t.Fatal(e)
	}
	if e := s.Unlink(h); e != nil {
		t.Fatal(e)
	}
	_, e := s.Write(h, []byte("destroyed"), 0, 0)
	requireErr(t, e, syscall.EACCES)
	requireErr(t, s.Truncate(h, 0), syscall.EACCES)
	requireErr(t, s.Setxattr(h, "user.destroy", []byte("x")), syscall.EACCES)
	if _, e = s.SetAttr(h, new(vfs.Attributes).SetUnixMode(0777)); !errors.Is(e, syscall.EACCES) {
		t.Fatalf("setattr=%v", e)
	}
	restored := fixtureWithStore(t, f.store, dump.Bytes())
	rh, e := restored.s.Open("protected", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	got := make([]byte, len(original))
	n, e := restored.s.Read(rh, got, 0, 0)
	if e != nil || !bytes.Equal(got[:n], original) {
		t.Fatalf("recovery=%q %v", got[:n], e)
	}
}
func TestReadOnlyMutations(t *testing.T) {
	f := newFixture(t)
	h := openFile(t, f.s, "data")
	if _, e := f.s.Write(h, []byte("ok"), 0, 1); e != nil {
		t.Fatal(e)
	}
	if e := f.s.Flush(h); e != nil {
		t.Fatal(e)
	}
	ro, e := New(f.native, true)
	if e != nil {
		t.Fatal(e)
	}
	defer ro.Shutdown()
	rh, e := ro.Open("data", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 2)
	if _, e = ro.Read(rh, b, 0, 0); e != nil || string(b) != "ok" {
		t.Fatalf("read=%q %v", b, e)
	}
	_, e = ro.Open("new", syscall.O_CREAT|syscall.O_RDWR, 0600)
	requireErr(t, e, syscall.EROFS)
	_, e = ro.Mkdir("newdir", 0700)
	requireErr(t, e, syscall.EROFS)
	_, e = ro.Write(rh, b, 0, 0)
	requireErr(t, e, syscall.EROFS)
	requireErr(t, ro.Truncate(rh, 0), syscall.EROFS)
	requireErr(t, ro.Rename(rh, "other", 1), syscall.EROFS)
	requireErr(t, ro.Unlink(rh), syscall.EROFS)
	requireErr(t, ro.Setxattr(rh, "user.x", b), syscall.EROFS)
	requireErr(t, ro.Removexattr(rh, "user.x"), syscall.EROFS)
	_, e = ro.SetAttr(rh, new(vfs.Attributes).SetUnixMode(0777))
	requireErr(t, e, syscall.EROFS)
	_, e = ro.Symlink(rh, "target", 1)
	requireErr(t, e, syscall.EROFS)
	_, e = ro.Link(vfs.VfsNode(2), vfs.VFS_ROOT_NODE, "hard")
	requireErr(t, e, syscall.EROFS)
}
func TestNativeSyncFlushCloseFailure(t *testing.T) {
	for _, operation := range []string{"sync-write", "flush", "close"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			flags := syscall.O_CREAT | syscall.O_RDWR
			if operation == "sync-write" {
				flags |= syscall.O_SYNC
			}
			h, e := f.s.Open("data", flags, 0600)
			if e != nil {
				t.Fatal(e)
			}
			f.store.fail.Store(true)
			if operation == "sync-write" {
				_, e = f.s.Write(h, []byte("fail"), 0, 0)
			} else {
				_, e = f.s.Write(h, []byte("fail"), 0, 0)
				if e != nil {
					t.Fatal(e)
				}
				if operation == "flush" {
					e = f.s.Flush(h)
				} else {
					e = f.s.Close(h)
				}
			}
			if e == nil {
				t.Fatal("acknowledged failed native object upload")
			}
		})
	}
}
