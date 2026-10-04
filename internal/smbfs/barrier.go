package smbfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

type diskBarrier struct{ path string }

// NewMetadataBarrier validates the existing SQLite path. No descriptor is kept.
func NewMetadataBarrier(metadataPath string) (MetadataBarrier, error) {
	if !filepath.IsAbs(metadataPath) {
		return nil, errors.New("metadata path must be absolute")
	}
	root, err := os.OpenRoot(filepath.Dir(metadataPath))
	if err != nil {
		return nil, err
	}
	info, statErr := root.Stat(filepath.Base(metadataPath))
	if err = errors.Join(statErr, root.Close()); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("metadata path must be a regular file")
	}
	return &diskBarrier{path: metadataPath}, nil
}

func (b *diskBarrier) Commit(ctx context.Context, _ bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// JuiceFS commits each SQLite transaction before returning. Its connection
	// hook enables fullfsync and checkpoint_fullfsync, including checkpoints that
	// remove the WAL while this barrier runs. Sync WAL first, then database and
	// directory so already committed transactions and their names are durable.
	root, err := os.OpenRoot(filepath.Dir(b.path))
	if err != nil {
		return err
	}
	name := filepath.Base(b.path)
	err = syncPath(ctx, root, name+"-wal", true)
	if err == nil {
		err = syncPath(ctx, root, name, false)
	}
	if err == nil {
		dir, openErr := root.Open(".")
		if openErr != nil {
			err = openErr
		} else {
			err = errors.Join(dir.Sync(), dir.Close())
		}
	}
	return errors.Join(err, root.Close(), ctx.Err())
}

func syncPath(ctx context.Context, root *os.Root, name string, optional bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// File.Sync uses F_FULLFSYNC on macOS for both sync modes.
	return errors.Join(file.Sync(), file.Close(), ctx.Err())
}
