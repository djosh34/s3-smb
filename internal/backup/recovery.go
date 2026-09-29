// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	const prefix = "meta/dump-"
	const suffix = ".json.gz"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return Point{}, errors.New("not a native metadata backup name")
	}
	ts := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	t, err := time.Parse("2006-01-02-150405", ts)
	if err != nil {
		return Point{}, errors.New("invalid native metadata backup timestamp")
	}
	return Point{key, t}, nil
}

// List returns native points newest-first. It does not silently select an older
// one if validation of the newest fails; selection/confirmation is caller-owned.
func List(ctx context.Context, blob object.ObjectStorage) ([]Point, error) {
	ch, err := object.ListAll(ctx, blob, "meta/dump-", "", true, false)
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
func inspectReader(r io.Reader) (*meta.Format, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var dump meta.DumpedMeta
	dec := json.NewDecoder(gz)
	if err = dec.Decode(&dump); err != nil {
		return nil, errors.New("invalid native metadata backup")
	}
	// Reading through EOF is necessary to verify the gzip checksum/trailer.
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid metadata backup trailer or trailing content")
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	if dump.Setting.UUID == "" || dump.Setting.Name == "" || dump.Counters == nil || dump.FSTree == nil || dump.FSTree.Attr == nil || dump.FSTree.Attr.Inode != meta.RootInode || dump.FSTree.Attr.Type != "directory" {
		return nil, errors.New("metadata backup is missing native identity or root")
	}
	if err = dump.Setting.CheckVersion(); err != nil {
		return nil, err
	}
	return &dump.Setting, nil
}
func Inspect(ctx context.Context, blob object.ObjectStorage, key string) (*meta.Format, error) {
	if _, err := parsePoint(key); err != nil {
		return nil, err
	}
	r, err := blob.Get(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	f, err := inspectReader(r)
	return f, errors.Join(err, r.Close(), ctx.Err())
}

// CleanupRecoveryStaging removes abandoned application recovery directories.
// Caller MUST hold the exclusive state lock, with no recovery worker alive.
// Unknown contents and symlinks are never traversed or removed.
func CleanupRecoveryStaging(stateDir string) error {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	known := map[string]bool{"selected.json.gz": true, "metadata.db": true, "metadata.db-wal": true, "metadata.db-shm": true}
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
		for _, file := range files {
			if err = os.Remove(filepath.Join(dir, file.Name())); err != nil {
				return err
			}
		}
		if err = os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

// Recover imports precisely the selected point into private, fresh SQLite. It
// publishes only after load, identity validation and database close/checkpoint.
// It never replaces an existing database. This is NOT a data-content scrub.
// Native LoadMeta is synchronous: lifecycle must arm its hard startup watchdog
// because context cancellation alone cannot interrupt the native import.
func Recover(ctx context.Context, blob object.ObjectStorage, key, dbPath string, current *meta.Format) (*meta.Format, error) {
	if current == nil {
		return nil, errors.New("current validated volume settings required")
	}
	if _, err := parsePoint(key); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dbPath); err == nil {
		return nil, errors.New("refusing recovery over existing database")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	parent := filepath.Dir(dbPath)
	dir, err := os.MkdirTemp(parent, ".s3-smb-recovery-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	stage, err := os.OpenFile(filepath.Join(dir, "selected.json.gz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	remote, err := blob.Get(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(stage, remote)
	if err = errors.Join(copyErr, remote.Close()); err != nil {
		return nil, err
	}
	if _, err = stage.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	saved, err := inspectReader(stage)
	if err != nil {
		return nil, err
	}
	if saved.UUID != current.UUID || saved.Name != current.Name || saved.BlockSize != current.BlockSize || saved.Compression != current.Compression || saved.HashPrefix != current.HashPrefix || saved.Shards != current.Shards || (saved.EncryptKey != "") != (current.EncryptKey != "") || saved.EncryptAlgo != current.EncryptAlgo {
		return nil, errors.New("recovery volume identity, layout or encryption mode mismatch")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "metadata.db")
	private, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = private.Close(); err != nil {
		return nil, err
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	m, err := meta.NewSQLite(path, conf)
	if err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = m.Shutdown()
		}
	}()
	if _, err = stage.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(stage)
	if err != nil {
		return nil, err
	}
	err = m.LoadMeta(gz)
	err = errors.Join(err, gz.Close())
	if err != nil {
		return nil, err
	}
	// Preserve native identity and layout but NEVER adopt old connection secrets
	// or destination. Transport/TLS remains owned by the already-open current store.
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
		return nil, err
	}
	var attr meta.Attr
	if st := m.GetAttr(meta.Background(), meta.RootInode, &attr); st != 0 {
		return nil, fmt.Errorf("recovered root: %w", st)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = m.Shutdown(); err != nil {
		return nil, err
	}
	closed = true
	// SQLite closes/checkpoints its WAL on the last connection. Never publish
	// only the main file if a nonempty WAL unexpectedly remains.
	if st, e := os.Stat(path + "-wal"); e == nil && st.Size() > 0 {
		return nil, errors.New("recovered SQLite still has an active WAL")
	} else if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Hard-link publication is atomic and fails if destination already exists.
	if err = os.Link(path, dbPath); err != nil {
		return nil, err
	}
	if err = syncDir(parent); err != nil {
		return nil, err
	}
	return saved, nil
}
