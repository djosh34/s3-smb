package smbfs

import (
	"errors"
	"sync/atomic"
	"syscall"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
)

type countedMetadata struct {
	meta.Meta
	attrs       atomic.Int64
	xattrs      atomic.Int64
	lookups     atomic.Int64
	directories atomic.Int64
	denyLoad    bool
}

func (m *countedMetadata) GetAttr(ctx meta.Context, ino meta.Ino, a *meta.Attr) syscall.Errno {
	m.attrs.Add(1)
	return m.Meta.GetAttr(ctx, ino, a)
}

func (m *countedMetadata) GetXattr(ctx meta.Context, ino meta.Ino, key string, value *[]byte) syscall.Errno {
	m.xattrs.Add(1)
	return m.Meta.GetXattr(ctx, ino, key, value)
}

func (m *countedMetadata) Lookup(ctx meta.Context, parent meta.Ino, name string, ino *meta.Ino, a *meta.Attr, check bool) syscall.Errno {
	m.lookups.Add(1)
	return m.Meta.Lookup(ctx, parent, name, ino, a, check)
}

func (m *countedMetadata) Readdir(ctx meta.Context, ino meta.Ino, plus uint8, entries *[]*meta.Entry) syscall.Errno {
	m.directories.Add(1)
	return m.Meta.Readdir(ctx, ino, plus, entries)
}

func (m *countedMetadata) Load(check bool) (*meta.Format, error) {
	if m.denyLoad {
		return nil, errors.New("unexpected format reload")
	}
	return m.Meta.Load(check)
}
