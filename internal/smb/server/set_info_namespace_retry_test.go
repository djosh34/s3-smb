package server

import (
	"context"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

// lookupMoveStorage changes real namespace entries after the discovery lookup.
// Data, attributes and every other operation still use the real adapter.
type lookupMoveStorage struct {
	smb.Storage
	move func(context.Context) error
	path string
	once sync.Once
}

func (s *lookupMoveStorage) Lookup(ctx context.Context, path string) (smb.Resolved, error) {
	resolved, err := s.Storage.Lookup(ctx, path)
	if err != nil || path != s.path {
		return resolved, err
	}
	var moveErr error
	s.once.Do(func() { moveErr = s.move(ctx) })
	if moveErr != nil {
		return smb.Resolved{}, moveErr
	}
	return resolved, nil
}

func TestRenameRetriesAfterSourceMoves(t *testing.T) {
	f := newNamespaceClient(t)
	f.create(t, "left", smb.KindDirectory)
	f.create(t, "right", smb.KindDirectory)
	source := f.open(t, "left/source", smb.KindFile, namespaceDeleteAccess|3, 7)
	f.write(t, source, "source bytes")
	storage := f.server.options.Storage
	f.server.options.Storage = &lookupMoveStorage{Storage: storage, path: "left/source", move: func(ctx context.Context) error {
		from, err := storage.Lookup(ctx, "left/source")
		if err != nil {
			return err
		}
		to, err := storage.Lookup(ctx, "right/intermediate")
		if err != nil {
			return err
		}
		return storage.Rename(ctx, smb.RenameRequest{Source: from.Name, Destination: to.Name, SourceInode: source.Object.Inode})
	}}
	namespaceStatus(t, f.rename(t, source, "right/final", false), smb.StatusSuccess)
	f.name(t, "left/source", 0)
	f.name(t, "right/intermediate", 0)
	f.name(t, "right/final", source.Object.Inode)
	f.data(t, source, "source bytes")
}

func TestRenameRetriesAfterDestinationParentMoves(t *testing.T) {
	f := newNamespaceClient(t)
	f.create(t, "left", smb.KindDirectory)
	right := f.create(t, "right", smb.KindDirectory)
	source := f.open(t, "left/source", smb.KindFile, namespaceDeleteAccess|3, 7)
	other := f.open(t, "right/other", smb.KindFile, 3, 7)
	f.write(t, source, "source bytes")
	f.write(t, other, "other bytes")
	storage := f.server.options.Storage
	f.server.options.Storage = &lookupMoveStorage{Storage: storage, path: "right/final", move: func(ctx context.Context) error {
		to, err := storage.Lookup(ctx, "oldright")
		if err != nil {
			return err
		}
		if err = storage.Rename(ctx, smb.RenameRequest{Source: right.Name, Destination: to.Name, SourceInode: right.Object.Inode}); err != nil {
			return err
		}
		_, err = storage.Create(ctx, right.Name, smb.KindDirectory)
		return err
	}}
	namespaceStatus(t, f.rename(t, source, "right/final", false), smb.StatusSuccess)
	f.name(t, "left/source", 0)
	f.name(t, "right/final", source.Object.Inode)
	f.name(t, "oldright/final", 0)
	f.name(t, "oldright/other", other.Object.Inode)
	f.data(t, source, "source bytes")
	f.data(t, other, "other bytes")
}
