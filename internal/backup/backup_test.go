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
)

// This test fixture extends the actual native file store with O_EXCL publication.
// It is NOT evidence for S3 semantics; Docker/MinIO acceptance is separate.
type fixtureStore struct {
	object.ObjectStorage
	dir  string
	puts atomic.Int32
	fail atomic.Bool
	lost atomic.Bool
}

func (s *fixtureStore) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	s.puts.Add(1)
	if s.fail.Load() {
		return errors.New("injected upload failure")
	}
	path := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, e := io.Copy(f, r)
	err = errors.Join(e, f.Close())
	if err == nil && s.lost.Load() {
		return errors.New("injected lost upload response")
	}
	return err
}
func newStore(t *testing.T) *fixtureStore {
	t.Helper()
	dir := t.TempDir() + "/"
	s, err := object.CreateStorage("file", dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureStore{ObjectStorage: s, dir: dir}
}
func newMetadata(t *testing.T) (meta.Meta, *meta.Format) {
	t.Helper()
	c := meta.DefaultConf()
	c.NoBGJob = true
	c.MaxDeletes = 0
	m, err := meta.NewSQLite(filepath.Join(t.TempDir(), "metadata.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown() })
	f := &meta.Format{Name: "fixture", UUID: "fixture-id", Storage: "s3", Bucket: "old-destination", AccessKey: "old-access", SecretKey: "old-secret", TrashDays: 14, BlockSize: 4096}
	if err = m.Init(f, false); err != nil {
		t.Fatal(err)
	}
	return m, f
}
func newManager(t *testing.T, m meta.Meta, s object.ObjectStorage, dir string, now func() time.Time, timeout time.Duration) *Manager {
	t.Helper()
	p, err := NewProtection(time.Hour, timeout, 14)
	if err != nil {
		t.Fatal(err)
	}
	p.now = now
	mgr, err := New(m, s, Options{StateDir: dir, Interval: time.Hour, Timeout: timeout, Attempts: 1, Protection: p})
	if err != nil {
		t.Fatal(err)
	}
	mgr.now = now
	return mgr
}
func readObject(t *testing.T, s object.ObjectStorage, key string) []byte {
	t.Helper()
	r, err := s.Get(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestNativeBackupRecoveryAndCurrentConnection(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
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
			now := time.Now().UTC().Truncate(time.Second)
			clock := func() time.Time { return now }
			dir := t.TempDir()
			mgr := newManager(t, m, s, dir, clock, 5*time.Second)
			if mgr.opts.Protection.Check() == nil {
				t.Fatal("new protection open before backup")
			}
			r, err := mgr.Backup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !r.Snapshot.Equal(now) {
				t.Fatal("receipt moved snapshot start")
			}
			if err = mgr.opts.Protection.Check(); err != nil {
				t.Fatal(err)
			}
			rawData := readObject(t, raw, r.Key)
			if encrypted && bytes.HasPrefix(rawData, []byte{0x1f, 0x8b}) {
				t.Fatal("encrypted export is plaintext gzip")
			}
			f2, err := Inspect(context.Background(), s, r.Key)
			if err != nil {
				t.Fatal(err)
			}
			if f2.UUID != f.UUID {
				t.Fatal("identity mismatch")
			}
			points, err := List(context.Background(), s)
			if err != nil || len(points) != 1 || points[0].Key != r.Key {
				t.Fatalf("points=%v %v", points, err)
			}
			restarted := newManager(t, m, s, dir, clock, 5*time.Second)
			ok, err := restarted.Reuse(context.Background())
			if err != nil || !ok {
				t.Fatalf("reuse=%v %v", ok, err)
			}
			if !restarted.receipt.Snapshot.Equal(r.Snapshot) {
				t.Fatal("restart postponed original schedule")
			}
			current := *f
			current.Bucket = "current-destination"
			current.AccessKey = "replacement-access"
			current.SecretKey = "replacement-secret"
			current.SessionToken = "replacement-token"
			path := filepath.Join(t.TempDir(), "restored.db")
			restored, err := Recover(context.Background(), s, r.Key, path, &current)
			if err != nil {
				t.Fatal(err)
			}
			if restored.Bucket != current.Bucket || restored.SecretKey != current.SecretKey || restored.SessionToken != current.SessionToken {
				t.Fatal("old connection won over current configuration")
			}
			recovered, err := meta.NewSQLite(path, meta.DefaultConf())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Shutdown()
			got, err := recovered.Load(false)
			if err != nil || got.UUID != current.UUID || got.SecretKey != current.SecretKey {
				t.Fatalf("persisted current format mismatch: %v", err)
			}
			// Native writable load: create a real inode and export another native point.
			var ino meta.Ino
			var attr meta.Attr
			if st := recovered.Create(meta.Background(), meta.RootInode, "after-recovery", 0644, 0, 0, &ino, &attr); st != 0 {
				t.Fatal(st)
			}
			now = now.Add(time.Second)
			next := newManager(t, recovered, s, t.TempDir(), clock, 5*time.Second)
			if _, err = next.Backup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err = Recover(context.Background(), s, r.Key, path, &current); err == nil {
				t.Fatal("replaced existing active database")
			}
			st, err := os.Stat(path)
			if err != nil || st.Mode().Perm() != 0600 {
				t.Fatalf("recovery mode %v %v", st, err)
			}
		})
	}
}
func TestBackupCollisionBackwardClockAndAmbiguousUpload(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	mgr := newManager(t, m, s, dir, clock, 5*time.Second)
	r, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	original := readObject(t, s, r.Key)
	for i := 0; i < 2; i++ {
		restart := newManager(t, m, s, dir, clock, 5*time.Second)
		if _, err = restart.Backup(context.Background()); err == nil {
			t.Fatal("collision succeeded")
		}
	}
	if s.puts.Load() != 1 || !bytes.Equal(original, readObject(t, s, r.Key)) {
		t.Fatal("collision overwrote prior backup")
	}
	now = now.Add(time.Second)
	s.lost.Store(true)
	ambiguous := newManager(t, m, s, dir, clock, 5*time.Second)
	if _, err = ambiguous.Backup(context.Background()); err == nil {
		t.Fatal("lost response reported success")
	}
	lostKey := "meta/dump-" + now.Format("2006-01-02-150405") + ".json.gz"
	lostData := readObject(t, s, lostKey)
	// Same name after restart must remain burned even if response was ambiguous.
	s.lost.Store(false)
	retry := newManager(t, m, s, dir, clock, 5*time.Second)
	if _, err = retry.Backup(context.Background()); err == nil {
		t.Fatal("ambiguous name reused")
	}
	if s.puts.Load() != 2 || !bytes.Equal(lostData, readObject(t, s, lostKey)) {
		t.Fatal("ambiguous object overwritten")
	}
	now = now.Add(-time.Second)
	backwards := newManager(t, m, s, dir, clock, 5*time.Second)
	if _, err = backwards.Backup(context.Background()); err == nil {
		t.Fatal("backward clock reused prior timestamp")
	}
	persisted, err := os.ReadFile(filepath.Join(dir, "backup-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), r.Key) {
		t.Fatal("failure replaced previous success receipt")
	}
	// Fresh recovery still restores the previous successful point.
	f, err := Inspect(context.Background(), s, r.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Recover(context.Background(), s, r.Key, filepath.Join(t.TempDir(), "restore.db"), f); err != nil {
		t.Fatal(err)
	}
}
func TestBackupRetriesAreBoundedAndPreserveReceipt(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	dir := t.TempDir()
	past := time.Now().Add(-time.Minute)
	first := newManager(t, m, s, dir, func() time.Time { return past }, 5*time.Second)
	r, err := first.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(filepath.Join(dir, "backup-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := newManager(t, m, s, dir, time.Now, 5*time.Second)
	mgr.opts.Attempts = 3
	s.fail.Store(true)
	if _, err = mgr.Backup(context.Background()); err == nil {
		t.Fatal("exhausted retries reported success")
	}
	if s.puts.Load() != 4 {
		t.Fatalf("expected one success then exactly three attempts; got %d", s.puts.Load())
	}
	current, err := os.ReadFile(filepath.Join(dir, "backup-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prior, current) {
		t.Fatal("failed retries replaced success evidence")
	}
	if mgr.opts.Protection.Check() == nil {
		t.Fatal("failed retries left gate open")
	}
	if _, err = Inspect(context.Background(), s, r.Key); err != nil {
		t.Fatal(err)
	}
}

func TestBackupScheduledFailureAndSuspension(t *testing.T) {
	for _, overdue := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-upload", true: "suspended"}[overdue], func(t *testing.T) {
			m, _ := newMetadata(t)
			s := newStore(t)
			var clock atomic.Int64
			clock.Store(time.Now().UTC().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			mgr := newManager(t, m, s, t.TempDir(), now, 5*time.Second)
			r, err := mgr.Backup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			step := time.Hour
			if overdue {
				step += 6 * time.Second
			}
			clock.Add(int64(step))
			s.fail.Store(true)
			if err = mgr.Run(context.Background()); err == nil {
				t.Fatal("scheduled failure/expiry kept writable serving")
			}
			if mgr.opts.Protection.Check() == nil {
				t.Fatal("failure reopened gate")
			}
			if _, err = Inspect(context.Background(), s, r.Key); err != nil {
				t.Fatal("previous backup lost", err)
			}
			if overdue && s.puts.Load() != 1 {
				t.Fatal("overdue operation attempted to revive expired protection")
			}
		})
	}
}

type stalledMeta struct {
	meta.Meta
	started, release chan struct{}
}

func (m stalledMeta) DumpMeta(w io.Writer, r meta.Ino, n int, s, f, t bool) error {
	close(m.started)
	<-m.release
	return m.Meta.DumpMeta(w, r, n, s, f, t)
}
func TestBackupTimeoutWaitJoinsNativeExport(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	stalled := stalledMeta{m, make(chan struct{}), make(chan struct{})}
	mgr := newManager(t, stalled, s, t.TempDir(), time.Now, 30*time.Millisecond)
	if _, err := mgr.Backup(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout=%v", err)
	}
	if mgr.opts.Protection.Check() == nil {
		t.Fatal("timeout left maintenance open")
	}
	joined := make(chan struct{})
	go func() { mgr.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("Wait released resources while export alive")
	case <-time.After(20 * time.Millisecond):
	}
	close(stalled.release)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("native export did not join")
	}
	if s.puts.Load() != 0 {
		t.Fatal("timed-out export uploaded")
	}
}
func TestInspectCorruptionDoesNotFallback(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	mgr := newManager(t, m, s, t.TempDir(), clock, 5*time.Second)
	first, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	second, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data := readObject(t, s, second.Key)
	if err = os.WriteFile(filepath.Join(s.dir, second.Key), data[:len(data)-4], 0600); err != nil {
		t.Fatal(err)
	}
	points, err := List(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if points[0].Key != second.Key {
		t.Fatal("latest point not selected")
	}
	if _, err = Inspect(context.Background(), s, points[0].Key); err == nil {
		t.Fatal("corrupt trailer accepted")
	}
	if _, err = Inspect(context.Background(), s, first.Key); err != nil {
		t.Fatal("prior point lost")
	}
}
func TestProtectionUsesWallTimeAndEffectiveRetention(t *testing.T) {
	for _, days := range []int{0, -1} {
		if _, err := NewProtection(time.Hour, time.Minute, days); err == nil {
			t.Fatal("unsafe retention allowed")
		}
	}
	if _, err := NewProtection(24*time.Hour, time.Minute, 1); err == nil {
		t.Fatal("cleanup overtakes protection")
	}
	p, err := NewProtection(time.Hour, time.Minute, 14)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.now = func() time.Time { return now }
	if err = p.protect(now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour + time.Minute)
	if p.Check() == nil {
		t.Fatal("suspended cleanup permitted")
	}
	now = now.Add(-2 * time.Hour)
	if p.Check() == nil {
		t.Fatal("backwards wall clock permitted")
	}
	p.Close()
	if p.protect(now) == nil {
		t.Fatal("fail-stop gate reopened")
	}
}
