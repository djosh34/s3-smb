// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/prometheus/client_golang/prometheus"
)

func OpenMetadata(path string, conf *meta.Config) (meta.Meta, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("metadata path must be absolute")
	}
	// O_EXCL preserves existing modes. The caller's private state directory and
	// state lock protect the SQLite/WAL files; existing files are never chmodded.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err = file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return meta.NewSQLite(path, conf)
}

// CacheConfig keeps native policy/defaults. Native CacheSize is already bytes:
// decimal input needs no MiB truncation, including a one-byte positive capacity.
func CacheConfig(format *meta.Format, dir string, capacity *int64) (chunk.Config, error) {
	if err := validateFormat(format); err != nil {
		return chunk.Config{}, err
	}
	c := chunk.Config{CacheDir: dir, CacheMode: 0600, CacheSize: 100 << 30, CacheChecksum: chunk.CsExtend, CacheScanInterval: time.Hour, FreeSpace: 0.1, AutoCreate: true, Compress: format.Compression, MaxUpload: 20, MaxDownload: 200, MaxRetries: 10, BlockSize: format.BlockSize << 10, GetTimeout: 60 * time.Second, PutTimeout: 60 * time.Second, CacheFullBlock: true, BufferSize: 300 << 20, Prefetch: 1, HashPrefix: format.HashPrefix}
	if capacity != nil {
		c.CacheSize = uint64(*capacity)
	}
	c.SelfCheck(format.UUID)
	return c, nil
}

type Runtime struct {
	FS       *fs.FileSystem
	Store    chunk.ChunkStore
	once     sync.Once
	closeErr error
}

// maintenanceStore is a final check at actual object deletion, including work
// queued while protection was still valid. It leaves native storage unchanged.
type maintenanceStore struct {
	object.ObjectStorage
	check func() error
}

func (s *maintenanceStore) Delete(ctx context.Context, key string, getters ...object.AttrGetter) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.ObjectStorage.Delete(ctx, key, getters...)
}

// OpenFilesystem registers the native CLI's delete/compact callbacks. It does
// not format metadata or start a session: lifecycle owns protection ordering.
func OpenFilesystem(m meta.Meta, blob object.ObjectStorage, format *meta.Format, cacheDir string, cacheBytes *int64, checkMaintenance func() error) (*Runtime, error) {
	c, err := CacheConfig(format, cacheDir, cacheBytes)
	if err != nil {
		return nil, err
	}
	store := chunk.NewCachedStore(&maintenanceStore{blob, checkMaintenance}, c, nil)
	m.OnMsg(meta.DeleteSlice, func(args ...interface{}) error {
		if err := checkMaintenance(); err != nil {
			return err
		}
		return store.Remove(args[0].(uint64), int(args[1].(uint32)))
	})
	m.OnMsg(meta.CompactChunk, func(args ...interface{}) error {
		if err := checkMaintenance(); err != nil {
			return err
		}
		return vfs.Compact(c, store, args[0].([]meta.Slice), args[1].(uint64), args[2].(uint8))
	})
	conf := &vfs.Config{Meta: meta.DefaultConf(), Format: *format, Chunk: &c, AttrTimeout: time.Second, EntryTimeout: time.Second, DirEntryTimeout: time.Second}
	filesystem, err := fs.NewFileSystem(conf, m, store, prometheus.NewRegistry())
	if err != nil {
		return nil, err
	}
	return &Runtime{FS: filesystem, Store: store}, nil
}

// Close follows native FS.Close, which flushes metadata and closes its session.
// All SMB handles must already be flushed/closed, and backup stopped/joined.
// The caller then owns Meta.Shutdown, transport Close, and state-lock release.
func (r *Runtime) Close() error { r.once.Do(func() { r.closeErr = r.FS.Close() }); return r.closeErr }
