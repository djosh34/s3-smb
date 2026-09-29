// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/thirdparty/xorm"
)

func protectedDB(t *testing.T, check func() error) *dbMeta {
	t.Helper()
	c := DefaultConf()
	c.NoBGJob = true
	c.MaxDeletes = 0
	c.CheckMaintenance = check
	mm, err := NewSQLite(filepath.Join(t.TempDir(), "meta.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	m := mm.(*dbMeta)
	t.Cleanup(func() { _ = m.Shutdown() })
	if err = m.Init(&Format{Name: "test", UUID: "test-uuid", TrashDays: 14, BlockSize: 4096}, false); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestSQLiteFullEveryConnection(t *testing.T) {
	for _, suffix := range []string{"", "?_sync=OFF&_synchronous=NORMAL"} {
		t.Run(suffix, func(t *testing.T) {
			mm, err := NewSQLite(filepath.Join(t.TempDir(), "meta.db")+suffix, DefaultConf())
			if err != nil {
				t.Fatal(err)
			}
			defer mm.Shutdown()
			m := mm.(*dbMeta)
			for round := 0; round < 2; round++ {
				var conns []*sql.Conn
				for i := 0; i < 4; i++ {
					c, e := m.db.DB().DB.Conn(context.Background())
					if e != nil {
						t.Fatal(e)
					}
					conns = append(conns, c)
					var sync int
					if e = c.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&sync); e != nil || sync != 2 {
						t.Fatalf("connection %d: synchronous=%d err=%v", i, sync, e)
					}
				}
				for _, c := range conns {
					c.Close()
				}
				m.db.DB().SetMaxIdleConns(0)
			}
		})
	}
}
func TestNativeRetirementGate(t *testing.T) {
	var expired atomic.Bool
	blocked := errors.New("expired test protection")
	m := protectedDB(t, func() error {
		if expired.Load() {
			return blocked
		}
		return nil
	})
	origin := marshalSlice(0, 100, 10, 0, 10)
	if _, err := m.db.Insert(&chunk{Inode: 2, Indx: 0, Slices: origin}, &sliceRef{Id: 100, Size: 10, Refs: 2}, &delslices{Id: 200, Deleted: 1, Slices: marshalDelayedSlices([]Slice{{Id: 100, Size: 10}})}); err != nil {
		t.Fatal(err)
	}
	expired.Store(true)
	if err := m.deleteChunk(2, 0); err == nil {
		t.Fatal("expired deleteChunk retired references")
	}
	if st := m.doCompactChunk(2, 0, origin, nil, 0, 0, 101, 10, nil); st == 0 {
		t.Fatal("expired compaction succeeded")
	}
	if _, err := m.doCleanupDelayedSlices(Background(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var c chunk
	ok, err := m.db.Where("inode = ? AND indx = ?", 2, 0).Get(&c)
	if err != nil || !ok || !bytes.Equal(c.Slices, origin) {
		t.Fatalf("native chunk changed: %v %v", ok, err)
	}
	var ref sliceRef
	ok, err = m.db.Where("chunkid = ?", 100).Get(&ref)
	if err != nil || !ok || ref.Refs != 2 {
		t.Fatalf("refs changed: %+v %v", ref, err)
	}
	if n, e := m.db.Count(&delslices{}); e != nil || n != 1 {
		t.Fatalf("delayed slice lost %d %v", n, e)
	}
	// Also check the transaction's final gate: expiry during native work rolls it back.
	expired.Store(false)
	err = m.maintenanceTxn(func(s *xorm.Session) error { _, e := s.Delete(&sliceRef{Id: 100}); expired.Store(true); return e })
	if !errors.Is(err, blocked) {
		t.Fatalf("expected final gate: %v", err)
	}
	if ok, _ = m.db.Get(&sliceRef{Id: 100}); !ok {
		t.Fatal("final gate failed to roll back")
	}
}

// Native delayed-slice representation is pairs of big-endian id and size.
func marshalDelayedSlices(ss []Slice) []byte {
	var b []byte
	for _, s := range ss {
		for shift := 56; shift >= 0; shift -= 8 {
			b = append(b, byte(s.Id>>shift))
		}
		for shift := 24; shift >= 0; shift -= 8 {
			b = append(b, byte(s.Size>>shift))
		}
	}
	return b
}
func TestQueuedNativeDeletionAfterSuspension(t *testing.T) {
	var expired atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	m := protectedDB(t, func() error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		if expired.Load() {
			return syscall.EROFS
		}
		return nil
	})
	if _, err := m.db.Insert(&sliceRef{Id: 9, Size: 4, Refs: 0}); err != nil {
		t.Fatal(err)
	}
	var deleted atomic.Int32
	m.OnMsg(DeleteSlice, func(args ...interface{}) error { deleted.Add(1); return nil })
	m.conf.MaxDeletes = 1
	m.sessCtx = Background()
	m.startDeleteSliceTasks()
	m.deleteSlice(9, 4)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("native deletion worker did not start")
	}
	expired.Store(true)
	close(release)
	m.stopDeleteSliceTasks()
	m.sessCtx.Cancel()
	m.sessWG.Wait()
	if deleted.Load() != 0 {
		t.Fatal("queued object deletion bypassed protection")
	}
	if ok, _ := m.db.Get(&sliceRef{Id: 9}); !ok {
		t.Fatal("queued reference deletion bypassed protection")
	}
}
func TestSQLiteExportConsistentDuringMutation(t *testing.T) {
	m := protectedDB(t, nil)
	if _, err := m.incrCounter("nextInode", 2); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var ino Ino = 2
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.txn(func(s *xorm.Session) error {
				if _, e := s.Insert(&node{Inode: ino, Type: TypeFile, Nlink: 1, Parent: 1}, &edge{Parent: 1, Name: []byte(fmt.Sprint(ino)), Inode: ino, Type: TypeFile}); e != nil {
					return e
				}
				_, e := s.Where("name = ?", "nextInode").Update(&counter{Value: int64(ino + 1)})
				return e
			})
			ino++
		}
	}()
	defer func() { close(stop); <-finished }()
	start := time.Now()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var size int
	for i := 0; i < 8; i++ {
		var out bytes.Buffer
		// false previously selected the mixed-transaction path; SQLite now forces
		// its native snapshot instead, and reports writer errors without fallback.
		if err := m.DumpMeta(&out, 0, 2, false, false, false); err != nil {
			t.Fatal(err)
		}
		var dump DumpedMeta
		if err := json.Unmarshal(out.Bytes(), &dump); err != nil {
			t.Fatal(err)
		}
		for _, e := range dump.FSTree.Entries {
			if e.Attr == nil || e.Attr.Inode >= Ino(dump.Counters.NextInode) {
				t.Fatalf("mixed transaction tree/counters: %+v %+v", e, dump.Counters)
			}
		}
		size = out.Len()
	}
	runtime.ReadMemStats(&after)
	t.Logf("8 native snapshots duration=%s final_json_bytes=%d allocations_bytes=%d heap_bytes=%d", time.Since(start), size, after.TotalAlloc-before.TotalAlloc, after.HeapAlloc)
	if err := m.DumpMeta(failingWriter{}, 0, 2, false, true, false); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("writer error lost: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("UNREGISTERED_PRIVATE_PANIC_MARKER") }
func TestSQLiteExportPanicDoesNotLeak(t *testing.T) {
	m := protectedDB(t, nil)
	var logs bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(oldLog)
	capture, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = capture
	defer func() { os.Stderr = oldStderr; capture.Close() }()
	err = m.DumpMeta(panicWriter{}, 0, 2, false, true, false)
	if err == nil || err.Error() != "native metadata export panicked" {
		t.Fatalf("unsafe error %v", err)
	}
	if _, err = capture.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(stderr) != 0 {
		t.Fatalf("raw stderr stack/payload: %q", stderr)
	}
	if strings.Contains(logs.String(), "UNREGISTERED_PRIVATE_PANIC_MARKER") || strings.Contains(logs.String(), "goroutine ") {
		t.Fatal("panic payload/stack leaked")
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid JSON diagnostic %q", line)
		}
	}
}

type stalledDeleteEngine struct {
	engine
	entered, release chan struct{}
}

func (e stalledDeleteEngine) doDeleteFileData(inode Ino, length uint64) {
	close(e.entered)
	<-e.release
	e.engine.doDeleteFileData(inode, length)
}
func TestCloseSessionJoinsActualFileDeletion(t *testing.T) {
	m := protectedDB(t, nil)
	e := stalledDeleteEngine{m.en, make(chan struct{}), make(chan struct{})}
	m.en = e
	m.tryDeleteFileData(999, 0, false)
	select {
	case <-e.entered:
	case <-time.After(time.Second):
		t.Fatal("native deletion did not start")
	}
	done := make(chan error, 1)
	go func() { done <- m.CloseSession() }()
	select {
	case <-done:
		t.Fatal("session closed while native deletion alive")
	case <-time.After(20 * time.Millisecond):
	}
	close(e.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deletion failed to join")
	}
	if m.startMutableTask(func() { t.Error("task admitted after unmount") }) {
		t.Fatal("new mutable task admitted after close")
	}
}

func TestCloseSessionJoinsRefresh(t *testing.T) {
	m := protectedDB(t, nil)
	m.conf.Heartbeat = time.Hour
	if err := m.NewSession(false); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := m.CloseSession(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("refresh sleep prevented bounded shutdown")
	}
	// Shutdown after CloseSession must not leave a refresh tail accessing SQL.
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
}
