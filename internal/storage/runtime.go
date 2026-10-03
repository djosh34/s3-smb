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
	// Create the database with mode 0600. An existing file keeps its mode.
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

// JuiceFS shares Meta.Retries between reads and flushes. At 53, download
// sleeps total 361.829s: sum((try-1)*300+1 ms, try=1..29), then 10s each.
// The flush deadline is max((Retries+2)^2/2 s, 300s) = 1512s, longer than
// eight minutes because reads need the same setting. Chunk uploads sleep
// try^2 seconds for try=0..MaxRetries; 12 gives 650s. All cover a 300s outage
// with margin, even when requests fail immediately.
const (
	filesystemRetries = 53
	uploadRetries     = 12
)

// CacheConfig returns the JuiceFS defaults with our data-path retry budget and
// the configured cache directory and size. The size is in bytes.
func CacheConfig(format *meta.Format, dir string, capacity *int64) (chunk.Config, error) {
	if err := validateFormat(format); err != nil {
		return chunk.Config{}, err
	}
	c := chunk.Config{CacheDir: dir, CacheMode: 0600, CacheSize: 100 << 30, CacheChecksum: chunk.CsExtend, CacheScanInterval: time.Hour, FreeSpace: 0.1, AutoCreate: true, Compress: format.Compression, MaxUpload: 20, MaxDownload: 200, MaxRetries: uploadRetries, BlockSize: format.BlockSize << 10, GetTimeout: 60 * time.Second, PutTimeout: 60 * time.Second, CacheFullBlock: true, BufferSize: 300 << 20, Prefetch: 1, HashPrefix: format.HashPrefix}
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

// maintenanceStore checks the delete guard again at each object deletion, so a
// delete queued while protection was open fails once it has closed.
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

// OpenFilesystem builds the chunk store and the JuiceFS filesystem and
// registers the delete and compact callbacks behind the delete guard. The
// caller starts the session afterwards.
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
	filesystem, err := fs.NewFileSystem(filesystemConfig(format, &c), m, store, prometheus.NewRegistry())
	if err != nil {
		return nil, err
	}
	return &Runtime{FS: filesystem, Store: store}, nil
}

func filesystemConfig(format *meta.Format, c *chunk.Config) *vfs.Config {
	conf := &vfs.Config{Meta: meta.DefaultConf(), Format: *format, Chunk: c, AttrTimeout: time.Second, EntryTimeout: time.Second, DirEntryTimeout: time.Second}
	conf.Meta.Retries = filesystemRetries
	return conf
}

// Close flushes JuiceFS metadata and closes its session. Call it after the SMB
// handles are closed and the backup loop has stopped.
func (r *Runtime) Close() error { r.once.Do(func() { r.closeErr = r.FS.Close() }); return r.closeErr }
