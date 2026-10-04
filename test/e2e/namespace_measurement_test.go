// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
)

type namespaceMeasurement struct {
	Bands                     uint64  `json:"bands"`
	SnapshotBytes             int64   `json:"snapshot_bytes"`
	BackupReceiptWriteSeconds float64 `json:"backup_receipt_write_seconds"`
	BackupStartupSeconds      float64 `json:"backup_startup_seconds"`
	RecoverySeconds           float64 `json:"recovery_seconds"`
	BackupPeakRSSBytes        int64   `json:"backup_peak_rss_bytes"`
	RecoveryPeakRSSBytes      int64   `json:"recovery_peak_rss_bytes"`
}

// TestNamespaceBackupMeasurements measures the metadata backup at startup and
// a full recovery for a large sparsebundle: in gate mode 524,288 eight-MiB
// bands, a fully allocated four-TiB bundle. It seeds metadata, not content; a
// real file checks the recovery. Both must take at most 60 seconds and 1 GiB
// of memory. The measurement is saved as namespace-measurement.json.
func TestNamespaceBackupMeasurements(t *testing.T) {
	bands := uint64(4096)
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		bands = 524288
	}
	f := newFixture(t, true)
	f.interval = "1h"
	f.startupTimeout = 3 * time.Minute
	d := f.start()
	share, disconnect := f.share()
	sentinel := map[string][]byte{"sentinel": []byte("content survives the large metadata snapshot\n")}
	writeFile(t, share, "sentinel", sentinel["sentinel"])
	disconnect()
	d.stop()
	seedBands(t, filepath.Join(f.root, "state", "metadata.db"), bands)
	receiptPath := filepath.Join(f.root, "state", "backup-receipt.json")
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	// Snapshot names have second precision, including the initial empty backup.
	time.Sleep(time.Second)

	// SMB readiness follows the startup backup, so this includes its retries,
	// receipt sync, rename and directory sync.
	backupStart := time.Now()
	d = f.start()
	startupTime := time.Since(backupStart)
	receipt := f.receipt()
	receiptStat, err := os.Stat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	head, err := f.store.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/" + receipt.Key)})
	if err != nil {
		t.Fatal(err)
	}
	backupRSS := daemonPeakRSS(d)
	d.stop()

	// Read-only startup recovers but takes no backup, so the time to SMB
	// readiness bounds the recovery time.
	f.freshLocal()
	f.readonly = true
	recoveryStart := time.Now()
	d = f.start()
	recoveryTime := time.Since(recoveryStart)
	recoveryRSS := daemonPeakRSS(d)
	share, disconnect = f.share()
	verifyFiles(t, share, sentinel)
	for _, i := range []uint64{0, bands / 2, bands - 1} {
		info, statErr := share.Stat(fmt.Sprintf("bands/%x", i))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Size() != 8<<20 {
			t.Fatalf("band %d size = %d, want 8 MiB", i, info.Size())
		}
	}
	disconnect()
	d.stop()

	measurement := namespaceMeasurement{
		Bands: bands, SnapshotBytes: aws.ToInt64(head.ContentLength),
		BackupReceiptWriteSeconds: receiptStat.ModTime().Sub(receipt.Snapshot).Seconds(),
		BackupStartupSeconds:      startupTime.Seconds(), RecoverySeconds: recoveryTime.Seconds(),
		BackupPeakRSSBytes: backupRSS, RecoveryPeakRSSBytes: recoveryRSS,
	}
	data, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("namespace measurement: %s", data)
	if err := f.logs.WriteFile("namespace-measurement.json", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if backupRSS > 1<<30 || recoveryRSS > 1<<30 {
		t.Errorf("daemon peak RSS exceeds 1 GiB: backup %d, recovery %d", backupRSS, recoveryRSS)
	}
	if startupTime > time.Minute || recoveryTime > time.Minute {
		t.Errorf("backup startup or recovery exceeds 60s: backup %s, recovery %s", startupTime, recoveryTime)
	}
}

// seedBands adds bands/1 to bands/<bands-1> (hex), each an eight-MiB file of
// two four-MiB slices, to the metadata database at path.
func seedBands(t *testing.T, path string, bands uint64) {
	t.Helper()
	dir, template := seedTemplate(t, path)
	// Clone the template's rows in one offline SQL transaction; JuiceFS would
	// take far too long for half a million files.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	err = cloneBands(t.Context(), db, uint64(dir), uint64(template), bands)
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
}

// seedTemplate uses JuiceFS to create the bands directory and the eight-MiB
// band 0, and returns their inodes.
func seedTemplate(t *testing.T, path string) (dir, template meta.Ino) {
	t.Helper()
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	m, err := meta.NewSQLite(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if shutdownErr := m.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	}()
	if _, err = m.Load(true); err != nil {
		t.Fatal(err)
	}
	var attr meta.Attr
	if st := m.Mkdir(meta.Background(), meta.RootInode, "bands", 0o755, 0, 0, &dir, &attr); st != 0 {
		t.Fatal(st)
	}
	if st := m.Create(meta.Background(), dir, "0", 0o644, 0, 0, &template, &attr); st != 0 {
		t.Fatal(st)
	}
	for i := range uint32(2) {
		var id uint64
		if st := m.NewSlice(meta.Background(), &id); st != 0 {
			t.Fatal(st)
		}
		if st := m.Write(meta.Background(), template, 0, i*(4<<20), meta.Slice{Id: id, Size: 4 << 20, Len: 4 << 20}, time.Now()); st != 0 {
			t.Fatal(st)
		}
	}
	return dir, template
}

// cloneBands copies the template inode, its directory entry and its chunk for
// each new band, with new slice IDs, and updates the counters.
func cloneBands(ctx context.Context, db *sql.DB, dir, template, bands uint64) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	var slices []byte
	if err = tx.QueryRowContext(ctx, `SELECT slices FROM jfs_chunk WHERE inode=? AND indx=0`, template).Scan(&slices); err != nil {
		return err
	}
	if len(slices) != 48 {
		return fmt.Errorf("template has %d slice bytes, want two 24-byte slices", len(slices))
	}
	var nextSlice uint64
	if err = tx.QueryRowContext(ctx, `SELECT value FROM jfs_counter WHERE name='nextChunk'`).Scan(&nextSlice); err != nil {
		return err
	}
	for i := uint64(1); i < bands; i++ {
		inode := template + i
		binary.BigEndian.PutUint64(slices[4:12], nextSlice+2*(i-1))
		binary.BigEndian.PutUint64(slices[28:36], nextSlice+2*(i-1)+1)
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO jfs_node SELECT ?,type,flags,mode,uid,gid,atime,mtime,ctime,atimensec,mtimensec,ctimensec,nlink,length,rdev,parent,access_acl_id,default_acl_id,tier_id FROM jfs_node WHERE inode=?`, []any{inode, template}},
			{`INSERT INTO jfs_edge(parent,name,inode,type) VALUES(?,?,?,1)`, []any{dir, []byte(strconv.FormatUint(i, 16)), inode}},
			{`INSERT INTO jfs_chunk(inode,indx,slices) VALUES(?,0,?)`, []any{inode, slices}},
		} {
			if _, err = tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
				return err
			}
		}
	}
	for _, counter := range []struct {
		query string
		name  string
		value uint64
	}{
		{`UPDATE jfs_counter SET value=MAX(value,?) WHERE name=?`, "nextInode", template + bands},
		{`UPDATE jfs_counter SET value=MAX(value,?) WHERE name=?`, "nextChunk", nextSlice + 2*(bands-1)},
		{`UPDATE jfs_counter SET value=value+? WHERE name=?`, "usedSpace", (bands - 1) * (8 << 20)},
		{`UPDATE jfs_counter SET value=value+? WHERE name=?`, "totalInodes", bands - 1},
	} {
		if _, err = tx.ExecContext(ctx, counter.query, counter.value, counter.name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func daemonPeakRSS(d *daemon) int64 {
	d.t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", d.cmd.Process.Pid))
	if err != nil {
		d.t.Fatal(err)
	}
	for line := range strings.Lines(string(status)) {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmHWM:" && fields[2] == "kB" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				d.t.Fatal(err)
			}
			return kb * 1024
		}
	}
	d.t.Fatal("daemon status has no VmHWM")
	return 0
}
