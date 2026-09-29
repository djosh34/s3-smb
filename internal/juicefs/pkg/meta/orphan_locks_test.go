// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Actual crash/reopen, including the caller-owned OS state lock. The child never
// closes its native session or handles: SIGKILL must leave persistent SID-zero
// locks, and startup must reclaim precisely those rows before serving again.
func TestReadOnlyOrphanLocksCrashRecovery(t *testing.T) {
	if dir := os.Getenv("S3_SMB_TEST_ORPHAN_LOCK_CHILD"); dir != "" {
		lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		conf := DefaultConf()
		conf.ReadOnly = true
		mm, err := NewSQLite(filepath.Join(dir, "metadata.db"), conf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = mm.Load(false); err != nil {
			t.Fatal(err)
		}
		if err = mm.NewSession(true); err != nil {
			t.Fatal(err)
		}
		m := mm.(*dbMeta)
		if m.sid != 0 {
			t.Fatalf("regression fixture needs native readonly SID zero; got %d", m.sid)
		}
		if st := m.Flock(Background(), 42, 1, F_WRLCK, false); st != 0 {
			t.Fatal(st)
		}
		if st := m.Setlk(Background(), 42, 1, false, F_WRLCK, 0, 100, 1); st != 0 {
			t.Fatal(st)
		}
		fmt.Println("native-locks-held")
		// Keep lock and metadata live until the parent kills this process.
		for {
			time.Sleep(time.Hour)
			_ = lock.Fd()
		}
	}
	for _, readOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("reopen-readonly-%v", readOnly), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "metadata.db")
			mm, err := NewSQLite(path, DefaultConf())
			if err != nil {
				t.Fatal(err)
			}
			m := mm.(*dbMeta)
			if err = m.Init(&Format{Name: "lock-test", UUID: "lock-test-id", TrashDays: 14, BlockSize: 4096}, false); err != nil {
				t.Fatal(err)
			}
			if _, err = m.db.Insert(&node{Inode: 42, Type: TypeFile, Mode: 0644, Nlink: 1, Parent: RootInode}, &edge{Parent: RootInode, Name: []byte("fixture"), Inode: 42, Type: TypeFile}, &flock{Inode: RootInode, Sid: 123, Owner: 999, Ltype: 'R'}); err != nil {
				t.Fatal(err)
			}
			if err = m.Shutdown(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestReadOnlyOrphanLocksCrashRecovery$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "S3_SMB_TEST_ORPHAN_LOCK_CHILD="+dir, "GOMAXPROCS=2")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			killed := false
			defer func() {
				if !killed {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			ready := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
			select {
			case line := <-ready:
				if line != "native-locks-held\n" {
					t.Fatalf("child readiness %q", line)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("child did not acquire native locks")
			}
			// Prove authority lock held before crash, rather than simulating restart in
			// the same process while the old writer could still mutate metadata.
			lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
				t.Fatal("live authority lock was not held")
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("child was not actually killed")
			}
			killed = true
			if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal("crashed authority lock not released", err)
			}
			defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			conf := DefaultConf()
			conf.ReadOnly = readOnly
			conf.NoBGJob = true
			mm, err = NewSQLite(path, conf)
			if err != nil {
				t.Fatal(err)
			}
			defer mm.Shutdown()
			m = mm.(*dbMeta)
			if _, err = m.Load(false); err != nil {
				t.Fatal(err)
			}
			if st := m.Flock(Background(), 42, 2, F_WRLCK, false); st != syscall.EAGAIN {
				t.Fatalf("crash did not preserve native flock: %v", st)
			}
			if st := m.Setlk(Background(), 42, 2, false, F_WRLCK, 0, 100, 2); st != syscall.EAGAIN {
				t.Fatalf("crash did not preserve native range lock: %v", st)
			}
			if err = ClearOrphanLocks(m); err != nil {
				t.Fatal(err)
			}
			if st := m.Flock(Background(), 42, 2, F_WRLCK, false); st != 0 {
				t.Fatalf("orphan flock not reclaimed: %v", st)
			}
			if st := m.Setlk(Background(), 42, 2, false, F_WRLCK, 0, 100, 2); st != 0 {
				t.Fatalf("orphan range lock not reclaimed: %v", st)
			}
			if ok, e := m.db.Get(&flock{Sid: 123, Owner: 999}); e != nil || !ok {
				t.Fatalf("registered session lock incorrectly swept: %v %v", ok, e)
			}
			var ino Ino
			var attr Attr
			if st := m.Create(Background(), RootInode, "should-not-create", 0644, 0, 0, &ino, &attr); readOnly && st != syscall.EROFS {
				t.Fatalf("lock cleanup weakened readonly namespace: %v", st)
			}
			if ok, e := m.db.Get(&node{Inode: 42}); e != nil || !ok {
				t.Fatalf("lock cleanup changed file metadata: %v %v", ok, e)
			}
		})
	}
}
