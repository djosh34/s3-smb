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
func inspectReader(r io.Reader) (*meta.Format, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var dump meta.DumpedMeta
	dec := json.NewDecoder(gz)
	if err = dec.Decode(&dump); err != nil {
		return nil, errors.New("invalid metadata backup")
	}
	// Read to EOF, so gzip verifies its checksum.
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid metadata backup trailer or trailing content")
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	if dump.Setting.UUID == "" || dump.Setting.Name == "" || dump.Counters == nil || dump.FSTree == nil || dump.FSTree.Attr == nil || dump.FSTree.Attr.Inode != meta.RootInode || dump.FSTree.Attr.Type != "directory" {
		return nil, errors.New("metadata backup has no volume identity or root directory")
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

// SameVolume reports whether two formats name the same volume with the same
// data layout and key.
func SameVolume(a, b *meta.Format) bool {
	return a != nil && b != nil && a.UUID == b.UUID && a.Name == b.Name && a.BlockSize == b.BlockSize && a.Compression == b.Compression && a.Shards == b.Shards && a.HashPrefix == b.HashPrefix && a.EncryptAlgo == b.EncryptAlgo && a.EncryptKey == b.EncryptKey
}

// CleanupRecoveryStaging removes staging directories left by an interrupted
// recovery. The caller holds the state lock. A directory with a file this
// package did not create, or with a symlink, stays as it is.
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

// Recover loads one metadata backup into a new SQLite file in a staging
// directory and links it to dbPath after the load, the identity check and a
// clean close. It fails when dbPath exists. It checks metadata only and reads
// no file data. The JuiceFS load cannot be cancelled through ctx.
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
	if !SameVolume(saved, current) {
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
	// Keep the identity and layout from the backup. Take the bucket, the
	// credentials and the retention from the current configuration, so an old
	// backup cannot redirect the store.
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
	// SQLite checkpoints its WAL when the last connection closes. A nonempty
	// WAL means the main file is incomplete, so do not link it into place.
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
	// A hard link is atomic and fails when dbPath already exists.
	if err = os.Link(path, dbPath); err != nil {
		return nil, err
	}
	if err = syncDir(parent); err != nil {
		return nil, err
	}
	return saved, nil
}
