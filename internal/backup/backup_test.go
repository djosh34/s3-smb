// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/storage"
)

// fixtureStore adds O_EXCL publication to the native file store. It says
// nothing about S3 semantics, which the MinIO tests cover.
type fixtureStore struct {
	object.ObjectStorage
	dir  string
	puts atomic.Int32
	fail atomic.Bool
	lost atomic.Bool
}

func (s *fixtureStore) PutIfAbsent(_ context.Context, key string, r io.Reader) error {
	s.puts.Add(1)
	if s.fail.Load() {
		return errors.New("injected upload failure")
	}
	path := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err = errors.Join(err, f.Close()); err == nil && s.lost.Load() {
		return errors.New("injected lost upload response")
	}
	return err
}

func newStore(t *testing.T) *fixtureStore {
	t.Helper()
	return openStore(t, t.TempDir())
}

func openStore(t *testing.T, dir string) *fixtureStore {
	t.Helper()
	s, err := object.CreateStorage("file", dir+string(os.PathSeparator), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureStore{ObjectStorage: s, dir: dir}
}

// cleanup runs fn at the end of the test and reports its error.
func cleanup(t *testing.T, fn func() error) {
	t.Cleanup(func() {
		if err := fn(); err != nil {
			t.Error(err)
		}
	})
}

type testMetadata struct {
	meta.Meta
	path string
}

func newMetadata(t *testing.T, maintenance ...func() error) (*testMetadata, *meta.Format) {
	t.Helper()
	c := meta.DefaultConf()
	if len(maintenance) > 0 {
		c.CheckMaintenance = maintenance[0]
	}
	c.NoBGJob = true
	c.MaxDeletes = 0
	path := filepath.Join(t.TempDir(), "metadata.db")
	m, err := meta.NewSQLite(path, c)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, m.Shutdown)
	f := &meta.Format{Name: "fixture", UUID: "1d8a6033-5fdd-4490-af83-bd2b21c9e682", Storage: "s3", Compression: "none", Bucket: "old-destination", AccessKey: "old-access", SecretKey: "old-secret", TrashDays: 14, BlockSize: 4096}
	if err = m.Init(f, false); err != nil {
		t.Fatal(err)
	}
	return &testMetadata{m, path}, f
}

// openMetadata opens a recovered database and loads its format.
func openMetadata(t *testing.T, path string, readonly bool, deletes int) *testMetadata {
	t.Helper()
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.ReadOnly = readonly
	conf.MaxDeletes = deletes
	m, err := storage.OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, func() error { return errors.Join(m.CloseSession(), m.Shutdown()) })
	if _, err = m.Load(true); err != nil {
		t.Fatal(err)
	}
	return &testMetadata{m, path}
}

type testClock struct{ ns atomic.Int64 }

func newClock() *testClock {
	c := &testClock{}
	c.ns.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	return c
}

func (c *testClock) now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) add(d time.Duration) { c.ns.Add(int64(d)) }

func newManager(t *testing.T, m *testMetadata, s object.ObjectStorage, dir string, now func() time.Time, timeout time.Duration) *Manager {
	t.Helper()
	p, err := NewProtection(time.Hour, timeout, 14)
	if err != nil {
		t.Fatal(err)
	}
	p.now = now
	mgr, err := New(m, s, Options{StateDir: dir, DatabasePath: m.path, Interval: time.Hour, Timeout: timeout, Protection: p})
	if err != nil {
		t.Fatal(err)
	}
	mgr.now = now
	t.Cleanup(mgr.Wait)
	return mgr
}

// backupOnce takes one backup of m into s with a fresh manager.
func backupOnce(t *testing.T, m *testMetadata, s object.ObjectStorage) Receipt {
	t.Helper()
	r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readObject(t *testing.T, s object.ObjectStorage, key string) []byte {
	t.Helper()
	r, err := s.Get(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err = errors.Join(err, r.Close()); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackupReuseAndRecover(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			testBackupReuseAndRecover(t, encrypted)
		})
	}
}

func testBackupReuseAndRecover(t *testing.T, encrypted bool) {
	ctx := context.Background()
	m, f := newMetadata(t)
	raw := newStore(t)
	var s object.ObjectStorage = raw
	if encrypted {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := object.NewDataEncryptor(object.NewRSAEncryptor(key), object.AES256GCM_RSA)
		if err != nil {
			t.Fatal(err)
		}
		s = object.NewEncrypted(raw, enc)
		f.EncryptKey = "bootstrap-protected-key"
		f.EncryptAlgo = object.AES256GCM_RSA
		if err = m.Init(f, false); err != nil {
			t.Fatal(err)
		}
	}
	clock := newClock()
	dir := t.TempDir()
	mgr := newManager(t, m, s, dir, clock.now, 5*time.Second)
	if mgr.opts.Protection.Check() == nil {
		t.Fatal("protection open before the first backup")
	}
	r, err := mgr.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Snapshot.Equal(clock.now()) {
		t.Fatal("receipt moved the snapshot start")
	}
	if err = mgr.opts.Protection.Check(); err != nil {
		t.Fatal(err)
	}
	if encrypted && bytes.HasPrefix(readObject(t, raw, r.Key), []byte{0x1f, 0x8b}) {
		t.Fatal("encrypted snapshot is plaintext gzip")
	}
	points, err := List(ctx, s)
	if err != nil || len(points) != 1 || points[0].Key != r.Key {
		t.Fatalf("points=%v %v", points, err)
	}
	saved, err := Inspect(ctx, s, r.Key, t.TempDir())
	if err != nil || saved.UUID != f.UUID {
		t.Fatalf("inspect: %v", err)
	}
	restarted := newManager(t, m, s, dir, clock.now, 5*time.Second)
	ok, err := restarted.Reuse(ctx)
	if err != nil || !ok {
		t.Fatalf("reuse=%v %v", ok, err)
	}
	if !restarted.receipt.Snapshot.Equal(r.Snapshot) {
		t.Fatal("restart postponed the schedule")
	}

	// The current configuration wins over the connection saved in the backup.
	current := *f
	current.Bucket = "current-destination"
	current.AccessKey = "replacement-access"
	current.SecretKey = "replacement-secret"
	current.SessionToken = "replacement-token"
	current.TrashDays = 7
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(ctx, s, r.Key, path, t.TempDir(), &current); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("recovered database mode %v %v", st, err)
	}
	recovered := openMetadata(t, path, false, 0)
	got, err := recovered.Load(false)
	if err != nil || got.UUID != current.UUID || got.Bucket != current.Bucket || got.SecretKey != current.SecretKey || got.SessionToken != current.SessionToken || got.TrashDays != current.TrashDays {
		t.Fatalf("persisted format does not match the current configuration: %v", err)
	}
	createInode(t, recovered, "after-recovery")
	clock.add(time.Second)
	if _, err = newManager(t, recovered, s, t.TempDir(), clock.now, 5*time.Second).Backup(ctx); err != nil {
		t.Fatal("backup after recovery:", err)
	}
	if _, err = Recover(ctx, s, r.Key, path, t.TempDir(), &current); err == nil {
		t.Fatal("recovery replaced an existing database")
	}
}

func TestBackupNeverReusesAName(t *testing.T) {
	ctx := context.Background()
	m, _ := newMetadata(t)
	s := newStore(t)
	dir := t.TempDir()
	clock := newClock()
	backup := func() error {
		mgr := newManager(t, m, s, dir, clock.now, 5*time.Second)
		mgr.wait = func(context.Context, time.Duration) error { return errors.New("no retry") }
		_, err := mgr.Backup(ctx)
		return err
	}
	if err := backup(); err != nil {
		t.Fatal(err)
	}
	if err := backup(); err == nil {
		t.Fatal("a restart in the same second reused the name")
	}
	// An upload whose response was lost burns its name across restarts.
	clock.add(time.Second)
	s.lost.Store(true)
	if err := backup(); err == nil {
		t.Fatal("lost upload response reported success")
	}
	lostKey := "meta/snapshot-" + clock.now().Format("2006-01-02-150405") + ".db.gz"
	lostData := readObject(t, s, lostKey)
	s.lost.Store(false)
	if err := backup(); err == nil {
		t.Fatal("ambiguous name reused")
	}
	clock.add(-time.Second)
	if err := backup(); err == nil {
		t.Fatal("backward clock reused an earlier name")
	}
	if s.puts.Load() != 2 || !bytes.Equal(lostData, readObject(t, s, lostKey)) {
		t.Fatalf("%d uploads; an existing object may have been overwritten", s.puts.Load())
	}
	persisted, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "backup-receipt.json")))
	if err != nil || strings.Contains(string(persisted), lostKey) {
		t.Fatalf("failure replaced the last good receipt: %v", err)
	}
}

type failingGetStore struct{ object.ObjectStorage }

func (failingGetStore) Get(context.Context, string, int64, int64, ...object.AttrGetter) (io.ReadCloser, error) {
	return nil, errors.New("injected transport failure")
}

func TestReuseFallsBackOrFails(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	for name, damage := range map[string]func(path string) error{
		"missing": os.Remove,
		"changed": func(path string) error { return os.WriteFile(path, []byte("other bytes"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newMetadata(t)
			s, dir := newStore(t), t.TempDir()
			r, err := newManager(t, m, s, dir, clock.now, 5*time.Second).Backup(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err = damage(filepath.Join(s.dir, filepath.FromSlash(r.Key))); err != nil {
				t.Fatal(err)
			}
			ok, err := newManager(t, m, s, dir, clock.now, 5*time.Second).Reuse(t.Context())
			if ok || err != nil {
				t.Fatalf("reuse=%v %v, want a new backup without an error", ok, err)
			}
		})
	}
	m, _ := newMetadata(t)
	s, dir := newStore(t), t.TempDir()
	if _, err := newManager(t, m, s, dir, clock.now, 5*time.Second).Backup(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := newManager(t, m, failingGetStore{s}, dir, clock.now, 5*time.Second).Reuse(ctx); ok || err == nil {
		t.Fatalf("reuse=%v %v, want the transport error", ok, err)
	}
	unformatted := &testMetadata{m, filepath.Join(t.TempDir(), "unformatted.db")}
	if err := os.WriteFile(unformatted.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := newManager(t, unformatted, s, dir, clock.now, 5*time.Second).Reuse(ctx); ok || err == nil {
		t.Fatalf("reuse=%v %v, want the metadata read error", ok, err)
	}
}

func createInode(t *testing.T, m meta.Meta, name string) meta.Ino {
	t.Helper()
	var ino meta.Ino
	var attr meta.Attr
	if st := m.Create(meta.Background(), meta.RootInode, name, 0o644, 0, 0, &ino, &attr); st != 0 {
		t.Fatal(st)
	}
	return ino
}

func queryCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := openSnapshotDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&count)
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	return count
}
