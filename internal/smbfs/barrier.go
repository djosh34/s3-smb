package smbfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type diskBarrier struct{ path string }

// NewMetadataBarrier validates the existing SQLite path. No descriptor is kept.
func NewMetadataBarrier(metadataPath string) (MetadataBarrier, error) {
	if !filepath.IsAbs(metadataPath) {
		return nil, fmt.Errorf("metadata path must be absolute")
	}
	info, err := os.Stat(metadataPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("metadata path must be a regular file")
	}
	return &diskBarrier{path: metadataPath}, nil
}

func (b *diskBarrier) Commit(ctx context.Context, full bool) error {
	// JuiceFS commits each SQLite transaction before returning. Its connection
	// hook enables fullfsync and checkpoint_fullfsync, including checkpoints that
	// remove the WAL while this barrier runs. Sync WAL first, then database and
	// directory so already committed transactions and their names are durable.
	if err := syncPath(ctx, b.path+"-wal", full, true); err != nil {
		return err
	}
	if err := syncPath(ctx, b.path, full, false); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(b.path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close(), ctx.Err())
}

func syncPath(ctx context.Context, path string, full, optional bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(syncFile(file, full), file.Close(), ctx.Err())
}
