/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package vfs

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	LastBackupTimeG = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "last_successful_backup",
		Help: "Last successful backup.",
	})
	LastBackupDurationG = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "last_backup_duration",
		Help: "Last backup duration.",
	})
)

// Backup metadata periodically in the object storage
func Backup(m meta.Meta, blob object.ObjectStorage, interval time.Duration, skipTrash bool) {
	ctx := meta.Background()
	key := "lastBackup"
	for {
		utils.SleepWithJitter(interval / 10)
		var value []byte
		if st := m.GetXattr(ctx, 0, key, &value); st != 0 && st != meta.ENOATTR {
			logger.Warnf("getxattr inode 1 key %s: %s", key, st)
			continue
		}
		var last time.Time
		var err error
		if len(value) > 0 {
			last, err = time.Parse(time.RFC3339, string(value))
		}
		if err != nil {
			logger.Warnf("invalid metadata backup timestamp")
			continue
		}
		if now := time.Now(); now.Sub(last) >= interval {
			var iused, dummy uint64
			_ = m.StatFS(ctx, meta.RootInode, &dummy, &dummy, &iused, &dummy)
			if iused >= 1e5 {
				logger.Infof("backup metadata started, inodes=%d", iused)
			}
			if fpath, err := backup(m, blob, now, true, skipTrash); err == nil {
				_ = m.SetXattr(ctx, 0, key, []byte(now.Format(time.RFC3339)), meta.XattrCreateOrReplace)
				go cleanupBackups(blob, now) // only cleanup on success
				LastBackupTimeG.Set(float64(now.UnixNano()) / 1e9)
				logger.Infof("backup metadata succeed, fast mode: %v, path: %q, used %s", iused < 1e5, fpath, time.Since(now))
			} else {
				logger.Warnf("backup metadata failed: %s", err)
			}
			LastBackupDurationG.Set(time.Since(now).Seconds())
		} else {
			LastBackupDurationG.Set(0)
		}
	}
}

func backup(m meta.Meta, blob object.ObjectStorage, now time.Time, fast, skipTrash bool) (string, error) {
	// Compatibility entry point. The embedding application uses BackupTo with
	// its private state directory and durable name reservations.
	dir, err := os.MkdirTemp("", "s3-smb-backup-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	key := "meta/dump-" + now.UTC().Format("2006-01-02-150405") + ".json.gz"
	_, err = BackupTo(context.Background(), m, blob, dir, key)
	return key, err
}

// WriteBackup keeps the native JSON/gzip format and checks the gzip trailer.
// Always export the whole tree including trash, using the consistent snapshot.
func WriteBackup(w io.Writer, m meta.Meta) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	err = m.DumpMeta(zw, 0, 2, false, true, false)
	return errors.Join(err, zw.Close())
}

// BackupTo performs one upload, never retries a PUT, and verifies full readback.
// Caller must hold the single-writer state lock and durably reserve key BEFORE
// calling: a lost PUT response permanently burns that name, even after restart.
// The supplied native store already handles prefixing and optional encryption.
func BackupTo(ctx context.Context, m meta.Meta, blob object.ObjectStorage, stagingDir, key string) (string, error) {
	if _, err := blob.Head(ctx, key); err == nil {
		return "", fmt.Errorf("metadata backup name already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	fp, err := os.CreateTemp(stagingDir, "export-*.json.gz")
	if err != nil {
		return "", err
	}
	defer os.Remove(fp.Name())
	defer fp.Close()
	if err = WriteBackup(fp, m); err != nil {
		return "", err
	}
	if err = fp.Sync(); err != nil {
		return "", err
	}
	if err = fp.Close(); err != nil {
		return "", err
	}
	fp, err = os.Open(fp.Name())
	if err != nil {
		return "", err
	}
	defer fp.Close()
	h := sha256.New()
	if _, err = io.Copy(h, fp); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if _, err = fp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = blob.Put(ctx, key, fp); err != nil {
		return "", err
	}
	r, err := blob.Get(ctx, key, 0, -1)
	if err != nil {
		return "", err
	}
	h.Reset()
	_, readErr := io.Copy(h, r)
	if err = errors.Join(readErr, r.Close()); err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return "", errors.New("metadata backup readback mismatch")
	}
	return digest, ctx.Err()
}

// CleanupBackups retains the native rotation policy. The caller's store must
// check live protection immediately before each actual Delete.
func CleanupBackups(ctx context.Context, blob object.ObjectStorage, now time.Time) error {
	blob = object.WithPrefix(blob, "meta/")
	ch, err := object.ListAll(ctx, blob, "", "", true, false)
	if err != nil {
		return err
	}
	var keys []string
	for o := range ch {
		if o == nil {
			return errors.New("metadata backup listing failed")
		}
		if !o.IsDir() && strings.HasPrefix(o.Key(), "dump-") && strings.HasSuffix(o.Key(), ".json.gz") {
			keys = append(keys, o.Key())
		}
	}
	for _, key := range rotate(keys, now) {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = blob.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func cleanupBackups(blob object.ObjectStorage, now time.Time) {
	blob = object.WithPrefix(blob, "meta/")
	ch, err := object.ListAll(context.TODO(), blob, "", "", true, false)
	if err != nil {
		logger.Warnf("listAll prefix meta/: %s", err)
		return
	}
	var objs []string
	for o := range ch {
		if o == nil {
			logger.Warnf("list failed, skip cleanup")
			return
		}
		if !o.IsDir() {
			objs = append(objs, o.Key())
		}
	}

	toDel := rotate(objs, now)
	for _, o := range toDel {
		if err = blob.Delete(context.Background(), o); err != nil {
			logger.Warnf("delete object %s: %s", o, err)
		}
	}
}

// Cleanup policy:
// 1. keep all backups within 2 days
// 2. keep one backup each day within 2 weeks
// 3. keep one backup each week within 2 months
// 4. keep one backup each month within 2 years
// 5. delete backups older than 2 years
func rotate(objs []string, now time.Time) []string {
	var days = 2
	cutoff := now.UTC().AddDate(-2, 0, 0)
	edge := now.UTC().AddDate(0, 0, -days)
	next := func() {
		if days < 14 {
			days++
			edge = edge.AddDate(0, 0, -1)
		} else if days < 60 {
			days += 7
			edge = edge.AddDate(0, 0, -7)
		} else {
			days += 30
			edge = edge.AddDate(0, 0, -30)
		}
	}

	var toDel, within []string
	sort.Strings(objs)
	for i := len(objs) - 1; i >= 0; i-- {
		if len(objs[i]) != 30 { // len("dump-2006-01-02-150405.json.gz")
			logger.Warnf("bad object for metadata backup %s: length %d", objs[i], len(objs[i]))
			continue
		}
		ts, err := time.Parse("2006-01-02-150405", objs[i][5:22])
		if err != nil {
			logger.Warnf("bad object for metadata backup %s: %s", objs[i], err)
			continue
		}

		if ts.Before(cutoff) {
			toDel = append(toDel, objs[:i+1]...)
			break
		}

		if ts.Before(edge) {
			if l := len(within); l > 0 { // keep the earliest one
				toDel = append(toDel, within[:l-1]...)
				within = within[:0]
			}
			for next(); ts.Before(edge); next() {
			}
			within = append(within, objs[i])
		} else if days > 2 {
			within = append(within, objs[i])
		}
	}
	if l := len(within); l > 0 {
		toDel = append(toDel, within[:l-1]...)
	}
	return toDel
}
