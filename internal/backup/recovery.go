// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type Point struct {
	Key  string
	Time time.Time
}

func parsePoint(key string) (Point, error) {
	const prefix = "meta/snapshot-"
	const suffix = ".db.gz"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return Point{}, errors.New("not a metadata backup name")
	}
	ts := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	t, err := time.Parse("2006-01-02-150405", ts)
	if err != nil {
		return Point{}, errors.New("invalid metadata backup timestamp")
	}
	return Point{key, t}, nil
}

// List returns the metadata backups in the bucket, newest first.
func List(ctx context.Context, blob object.ObjectStorage) ([]Point, error) {
	ch, err := object.ListAll(ctx, blob, "meta/snapshot-", "", true, false)
	if err != nil {
		return nil, err
	}
	var points []Point
	for o := range ch {
		if o == nil {
			return nil, errors.New("metadata backup listing failed")
		}
		if !o.IsDir() {
			if p, e := parsePoint(o.Key()); e == nil {
				points = append(points, p)
			}
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Time.After(points[j].Time) })
	return points, ctx.Err()
}

// Inspect reads the identity from a validated snapshot without creating a local
// metadata database or opening the snapshot writable. The caller holds the state
// lock; interrupted inspection files are covered by CleanupRecoveryStaging.
func Inspect(ctx context.Context, blob object.ObjectStorage, key, stateDir string) (format *meta.Format, err error) {
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("absolute state directory required for snapshot inspection")
	}
	if _, err = parsePoint(key); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(stateDir, ".s3-smb-recovery-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "metadata.db")
	if err = downloadSnapshot(ctx, blob, key, path); err != nil {
		return nil, err
	}
	return inspectSnapshot(ctx, path)
}

// SameVolume reports whether two formats name the same volume with the same
// data layout and key.
func SameVolume(a, b *meta.Format) bool {
	return a != nil && b != nil && a.UUID == b.UUID && a.Name == b.Name && a.BlockSize == b.BlockSize && a.Compression == b.Compression && a.Shards == b.Shards && a.HashPrefix == b.HashPrefix && a.EncryptAlgo == b.EncryptAlgo && a.EncryptKey == b.EncryptKey
}

// CleanupRecoveryStaging removes staging directories left by an interrupted
// recovery. The caller holds the state lock. Unknown files and symlinks stay.
func CleanupRecoveryStaging(stateDir string) error {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	known := map[string]bool{"metadata.db": true, "metadata.db-wal": true, "metadata.db-shm": true}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".s3-smb-recovery-") {
			continue
		}
		dir := filepath.Join(stateDir, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		owned := true
		for _, file := range files {
			if !known[file.Name()] || !file.Type().IsRegular() {
				owned = false
				break
			}
		}
		if !owned {
			slog.Warn("unknown recovery staging contents left untouched")
			continue
		}
		if err = os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// Recover validates a snapshot and clears dead sessions in a staged copy. The
// caller holds the state lock and has confirmed the old writer has stopped.
// After preparation, recovery wipes the volume cache and renames the database.
func Recover(ctx context.Context, blob object.ObjectStorage, key, dbPath, cacheRoot string, current *meta.Format) (*meta.Format, error) {
	return recoverMetadata(ctx, blob, key, dbPath, current, func() error {
		return WipeVolumeCache(cacheRoot, current.UUID, filepath.Dir(dbPath))
	})
}

func recoverMetadata(ctx context.Context, blob object.ObjectStorage, key, dbPath string, current *meta.Format, beforePublish func() error) (saved *meta.Format, err error) {
	if current == nil {
		return nil, errors.New("current validated volume settings required")
	}
	if _, err = parsePoint(key); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(dbPath); err == nil {
		return nil, errors.New("refusing recovery over existing database")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	parent := filepath.Dir(dbPath)
	dir, err := os.MkdirTemp(parent, ".s3-smb-recovery-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "metadata.db")
	if err = downloadSnapshot(ctx, blob, key, path); err != nil {
		return nil, err
	}
	saved, err = inspectSnapshot(ctx, path)
	if err != nil {
		return nil, err
	}
	if !SameVolume(saved, current) {
		return nil, errors.New("recovery volume identity, layout or encryption mode mismatch")
	}
	if err = prepareSnapshot(ctx, path, saved, current); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// A kill before the rename leaves no database. The next start recovers
	// again, including another cache wipe.
	if err = beforePublish(); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = os.Rename(path, dbPath); err != nil {
		return nil, err
	}
	if err = syncDir(parent); err != nil {
		return nil, err
	}
	return saved, nil
}

func prepareSnapshot(ctx context.Context, path string, saved, current *meta.Format) (err error) {
	if err = expireSnapshotSessions(ctx, path); err != nil {
		return err
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	m, err := meta.NewSQLite(path, conf)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, m.CloseSession(), m.Shutdown())
		}
	}()
	if _, err = m.Load(true); err != nil {
		return err
	}
	// Preserve identity and layout. The current config sets the destination,
	// credentials, key and retention, so a backup cannot redirect the store.
	saved.Storage = current.Storage
	saved.StorageClass = current.StorageClass
	saved.Tiers = current.Tiers
	saved.Bucket = current.Bucket
	saved.AccessKey = current.AccessKey
	saved.SecretKey = current.SecretKey
	saved.SessionToken = current.SessionToken
	saved.EncryptKey = current.EncryptKey
	saved.KeyEncrypted = current.KeyEncrypted
	saved.TrashDays = current.TrashDays
	saved.RangerRestUrl = current.RangerRestUrl
	saved.RangerService = current.RangerService
	saved.KerbConf = current.KerbConf
	if err = m.Init(saved, false); err != nil {
		return err
	}
	if err = cleanSnapshotSessions(ctx, m, path); err != nil {
		return err
	}
	var attr meta.Attr
	if st := m.GetAttr(meta.Background(), meta.RootInode, &attr); st != 0 {
		return fmt.Errorf("recovered root: %w", st)
	}
	// Session cleanup can queue inode retirement even with background jobs off.
	// Join it and flush the resulting counters before closing SQLite.
	if err = m.CloseSession(); err != nil {
		return err
	}
	err = m.Shutdown()
	closed = true
	if err != nil {
		return err
	}
	// Last-connection close checkpoints SQLite's WAL. Do not publish an
	// incomplete main file if a WAL is unexpectedly still active.
	if st, e := os.Stat(path + "-wal"); e == nil && st.Size() > 0 {
		return errors.New("recovered SQLite still has an active WAL")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close(), ctx.Err())
}
