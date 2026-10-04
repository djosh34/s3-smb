// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
)

// 524,288 eight-MiB bands describe a fully allocated four-TiB sparsebundle.
// Seed metadata, not four TiB of content. A real SMB sentinel checks recovery.
func TestNamespaceBackupMeasurements(t *testing.T) {
	bands := 4096
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		bands = 524288
	}
	f := newFixture(t, true)
	f.interval = "1h"
	f.startupTimeout = 3 * time.Minute
	d := f.start()
	s, closeShare := f.share()
	sentinel := map[string][]byte{"sentinel": []byte("content survives the large metadata snapshot\n")}
	writeFile(t, s, "sentinel", sentinel["sentinel"])
	closeShare()
	d.stop()
	seedBands(t, filepath.Join(f.root, "state", "metadata.db"), bands)
	if err := os.Remove(filepath.Join(f.root, "state", "backup-receipt.json")); err != nil {
		t.Fatal(err)
	}
	// Snapshot names have second precision, including the initial empty backup.
	time.Sleep(time.Second)

	backupStart := time.Now()
	d, backupDisk := measuredStart(t, f)
	startupTime := time.Since(backupStart)
	r := f.receipt()
	receiptStat, err := os.Stat(filepath.Join(f.root, "state", "backup-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	backupTime := receiptStat.ModTime().Sub(r.Snapshot)
	head, err := f.store.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/" + r.Key)})
	if err != nil {
		t.Fatal(err)
	}
	backupRSS := daemonPeakRSS(d)
	d.stop()

	f.freshLocal()
	// Read-only startup includes identity inspection and full recovery, but no
	// extra backup. Time to SMB readiness is an upper bound on recovery time.
	f.readonly = true
	recoveryStart := time.Now()
	d, recoveryDisk := measuredStart(t, f)
	recoveryTime := time.Since(recoveryStart)
	recoveryRSS := daemonPeakRSS(d)
	s, closeShare = f.share()
	verifyFiles(t, s, sentinel)
	for _, i := range []int{0, bands / 2, bands - 1} {
		info, e := s.Stat(fmt.Sprintf("bands/%x", i))
		if e != nil {
			t.Fatal(e)
		}
		if info.Size() != 8<<20 {
			t.Fatalf("band %d size = %d, want 8 MiB", i, info.Size())
		}
	}
	closeShare()
	d.stop()
	measurement := struct {
		Bands                int     `json:"bands"`
		SnapshotBytes        int64   `json:"snapshot_bytes"`
		BackupSeconds        float64 `json:"backup_seconds"`
		BackupStartupSeconds float64 `json:"backup_startup_seconds"`
		RecoverySeconds      float64 `json:"recovery_seconds"`
		BackupPeakRSSBytes   int64   `json:"backup_peak_rss_bytes"`
		RecoveryPeakRSSBytes int64   `json:"recovery_peak_rss_bytes"`
		BackupStagingBytes   int64   `json:"backup_staging_bytes"`
		RecoveryStagingBytes int64   `json:"recovery_staging_bytes"`
	}{bands, aws.ToInt64(head.ContentLength), backupTime.Seconds(), startupTime.Seconds(), recoveryTime.Seconds(), backupRSS, recoveryRSS, backupDisk, recoveryDisk}
	data, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("namespace measurement: %s", data)
	if artifacts := os.Getenv("S3_SMB_TEST_ARTIFACTS"); artifacts != "" {
		if err = os.WriteFile(filepath.Join(artifacts, "namespace-measurement.json"), append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Both modes enforce the same ceilings. Gate mode supplies the scale.
	if backupRSS > 1<<30 || recoveryRSS > 1<<30 {
		t.Errorf("daemon peak RSS exceeds 1 GiB: backup=%d recovery=%d", backupRSS, recoveryRSS)
	}
	if backupTime > time.Minute || recoveryTime > time.Minute {
		t.Errorf("snapshot or recovery exceeds 60s: backup=%s recovery=%s", backupTime, recoveryTime)
	}
}

func seedBands(t *testing.T, path string, bands int) {
	t.Helper()
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	m, err := meta.NewSQLite(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			if err := m.Shutdown(); err != nil {
				t.Error(err)
			}
		}
	}()
	if _, err = m.Load(true); err != nil {
		t.Fatal(err)
	}
	var dir, template meta.Ino
	var attr meta.Attr
	if st := m.Mkdir(meta.Background(), meta.RootInode, "bands", 0755, 0, 0, &dir, &attr); st != 0 {
		t.Fatal(st)
	}
	if st := m.Create(meta.Background(), dir, "0", 0644, 0, 0, &template, &attr); st != 0 {
		t.Fatal(st)
	}
	// Use JuiceFS to establish a normal eight-MiB inode and two four-MiB
	// slices. The remaining rows are cloned in one offline SQL transaction.
	for i := uint32(0); i < 2; i++ {
		var id uint64
		if st := m.NewSlice(meta.Background(), &id); st != 0 {
			t.Fatal(st)
		}
		if st := m.Write(meta.Background(), template, 0, i*(4<<20), meta.Slice{Id: id, Size: 4 << 20, Len: 4 << 20}, time.Now()); st != 0 {
			t.Fatal(st)
		}
	}
	if err = m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	closed = true
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	nodes, err := tx.Prepare(`INSERT INTO jfs_node SELECT ?,type,flags,mode,uid,gid,atime,mtime,ctime,atimensec,mtimensec,ctimensec,nlink,length,rdev,parent,access_acl_id,default_acl_id,tier_id FROM jfs_node WHERE inode=?`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodes.Close(); err != nil {
			t.Error(err)
		}
	}()
	edges, err := tx.Prepare(`INSERT INTO jfs_edge(parent,name,inode,type) VALUES(?,?,?,1)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := edges.Close(); err != nil {
			t.Error(err)
		}
	}()
	chunks, err := tx.Prepare(`INSERT INTO jfs_chunk(inode,indx,slices) VALUES(?,0,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := chunks.Close(); err != nil {
			t.Error(err)
		}
	}()
	var slices []byte
	if err = tx.QueryRow(`SELECT slices FROM jfs_chunk WHERE inode=? AND indx=0`, template).Scan(&slices); err != nil {
		t.Fatal(err)
	}
	if len(slices) != 48 {
		t.Fatalf("template has %d slice bytes, want two 24-byte slices", len(slices))
	}
	var nextSlice uint64
	if err = tx.QueryRow(`SELECT value FROM jfs_counter WHERE name='nextChunk'`).Scan(&nextSlice); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < bands; i++ {
		inode := uint64(template) + uint64(i)
		if _, err = nodes.Exec(inode, template); err != nil {
			t.Fatal(err)
		}
		if _, err = edges.Exec(dir, []byte(fmt.Sprintf("%x", i)), inode); err != nil {
			t.Fatal(err)
		}
		binary.BigEndian.PutUint64(slices[4:12], nextSlice+uint64(2*(i-1)))
		binary.BigEndian.PutUint64(slices[28:36], nextSlice+uint64(2*(i-1)+1))
		if _, err = chunks.Exec(inode, slices); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]uint64{"nextInode": uint64(template) + uint64(bands), "nextChunk": nextSlice + uint64(2*(bands-1))} {
		if _, err = tx.Exec(`UPDATE jfs_counter SET value=MAX(value,?) WHERE name=?`, value, name); err != nil {
			t.Fatal(err)
		}
	}
	for name, delta := range map[string]uint64{"usedSpace": uint64(bands-1) * (8 << 20), "totalInodes": uint64(bands - 1)} {
		if _, err = tx.Exec(`UPDATE jfs_counter SET value=value+? WHERE name=?`, delta, name); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func daemonPeakRSS(d *daemon) int64 {
	d.t.Helper()
	file, err := os.Open(fmt.Sprintf("/proc/%d/status", d.cmd.Process.Pid))
	if err != nil {
		d.t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			d.t.Error(err)
		}
	}()
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) == 3 && fields[0] == "VmHWM:" && fields[2] == "kB" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				d.t.Fatal(err)
			}
			return kb * 1024
		}
	}
	if err = scan.Err(); err != nil {
		d.t.Fatal(err)
	}
	d.t.Fatal("daemon status has no VmHWM")
	return 0
}

// Sample allocated staging blocks every 5ms. Missing files are normal when the
// daemon removes or renames them; other filesystem errors fail the measurement.
func stagingBytes(root string) (int64, error) {
	state := filepath.Join(root, "state")
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if entry.Name() != "backup-staging" && !strings.HasPrefix(entry.Name(), ".s3-smb-recovery-") {
			continue
		}
		err = filepath.WalkDir(filepath.Join(state, entry.Name()), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("no allocated-block count for %s", path)
			}
			total += stat.Blocks * 512
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func measuredStart(t *testing.T, f *fixture) (*daemon, int64) {
	t.Helper()
	done := make(chan struct{})
	type observation struct {
		peak int64
		err  error
	}
	result := make(chan observation, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		var peak int64
		for {
			bytes, err := stagingBytes(f.root)
			if err != nil {
				result <- observation{peak, err}
				return
			}
			if bytes > peak {
				peak = bytes
			}
			select {
			case <-done:
				result <- observation{peak, nil}
				return
			case <-ticker.C:
			}
		}
	}()
	joined := false
	defer func() {
		if !joined {
			close(done)
			<-result
		}
	}()
	d := f.start()
	close(done)
	got := <-result
	joined = true
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.peak == 0 {
		t.Fatal("no staging disk use observed")
	}
	return d, got.peak
}
