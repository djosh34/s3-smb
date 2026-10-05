// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
)

// OpenMetadata opens the SQLite metadata database at an absolute path. It
// creates a missing database with mode 0600 and keeps the mode of an existing one.
func OpenMetadata(path string, conf *meta.Config) (meta.Meta, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("metadata path must be absolute")
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
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
// sleeps for one slice reader at a time total 361.829s:
// sum((try-1)*300+1 ms, try=1..29), then 10s each.
// The flush deadline is max((Retries+2)^2/2 s, 300s) = 1512s, longer than
// eight minutes because reads need the same setting. Chunk uploads sleep
// try^2 seconds for try=0..MaxRetries; 12 gives 650s. All cover a 300s outage
// with margin, even when requests fail immediately. Concurrent block reads
// share one retry counter per open file, so the SMB adapter retries reads too.
const (
	FilesystemRetries = 53
	UploadRetries     = 12
)

// CacheConfig returns the JuiceFS defaults with our data-path retry budget and
// the configured cache directory and size. The size is in bytes; nil keeps the
// 100 GiB default.
func CacheConfig(format *meta.Format, dir string, capacity *uint64) (chunk.Config, error) {
	if err := validateFormat(format); err != nil {
		return chunk.Config{}, err
	}
	c := chunk.Config{CacheDir: dir, CacheMode: 0o600, CacheSize: 100 << 30, CacheChecksum: chunk.CsExtend, CacheScanInterval: time.Hour, FreeSpace: 0.1, AutoCreate: true, Compress: format.Compression, MaxUpload: 4, MaxDownload: 200, MaxRetries: UploadRetries, BlockSize: format.BlockSize << 10, GetTimeout: 60 * time.Second, PutTimeout: 60 * time.Second, CacheFullBlock: true, BufferSize: 300 << 20, Prefetch: 1, HashPrefix: format.HashPrefix}
	if capacity != nil {
		c.CacheSize = *capacity
	}
	c.SelfCheck(format.UUID)
	return c, nil
}

// Runtime is an open JuiceFS filesystem with its chunk store.
type Runtime struct {
	FS    *fs.FileSystem
	Store chunk.ChunkStore
	// Config supplies the same I/O settings to the SMB filesystem adapter.
	Config   *vfs.Config
	closeErr error
	once     sync.Once
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
func OpenFilesystem(m meta.Meta, blob object.ObjectStorage, format *meta.Format, cacheDir string, cacheBytes *uint64, checkMaintenance func() error) (*Runtime, error) {
	c, err := CacheConfig(format, cacheDir, cacheBytes)
	if err != nil {
		return nil, err
	}
	store := chunk.NewCachedStore(&maintenanceStore{blob, checkMaintenance}, c, nil)
	m.OnMsg(meta.DeleteSlice, func(args ...any) error {
		if denied := checkMaintenance(); denied != nil {
			return denied
		}
		if len(args) != 2 {
			return errors.New("unexpected slice deletion arguments")
		}
		id, idOK := args[0].(uint64)
		size, sizeOK := args[1].(uint32)
		if !idOK || !sizeOK {
			return errors.New("unexpected slice deletion arguments")
		}
		return store.Remove(id, int(size))
	})
	m.OnMsg(meta.CompactChunk, func(args ...any) error {
		if denied := checkMaintenance(); denied != nil {
			return denied
		}
		if len(args) != 3 {
			return errors.New("unexpected chunk compaction arguments")
		}
		slices, slicesOK := args[0].([]meta.Slice)
		id, idOK := args[1].(uint64)
		tier, tierOK := args[2].(uint8)
		if !slicesOK || !idOK || !tierOK {
			return errors.New("unexpected chunk compaction arguments")
		}
		return vfs.Compact(c, store, slices, id, tier)
	})
	conf := filesystemConfig(format, &c)
	filesystem, err := fs.NewFileSystem(conf, m, store, prometheus.NewRegistry())
	if err != nil {
		return nil, err
	}
	return &Runtime{FS: filesystem, Store: store, Config: conf}, nil
}

func filesystemConfig(format *meta.Format, c *chunk.Config) *vfs.Config {
	conf := &vfs.Config{Meta: meta.DefaultConf(), Format: *format, Chunk: c, AttrTimeout: time.Second, EntryTimeout: time.Second, DirEntryTimeout: time.Second}
	conf.Meta.Retries = FilesystemRetries
	return conf
}

// Close flushes JuiceFS metadata and closes its session. Call it after the SMB
// handles are closed and the backup loop has stopped.
func (r *Runtime) Close() error {
	r.once.Do(func() { r.closeErr = r.FS.Close() })
	return r.closeErr
}
