package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// MS-FSA 2.1.5.14.2 limits client-settable attributes to these seven bits.
const clientSettableFileAttributes uint32 = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000020 | 0x00000100 | 0x00001000 | 0x00002000

func normalizeFileAttributes(attributes uint32) uint32 {
	attributes &= clientSettableFileAttributes
	if attributes == 0 {
		return 0x80 // FILE_ATTRIBUTE_NORMAL.
	}
	return attributes
}

func setCreateAttributes(ctx context.Context, storage smb.Storage, create wire.CreateRequest, resolved smb.Resolved, action uint32) error {
	if action == 1 || create.FileAttributes == 0 {
		return nil
	}
	attributes := create.FileAttributes
	if action == 3 {
		attributes |= resolved.Attr.Attributes
	}
	attributes = normalizeFileAttributes(attributes)
	return storage.SetAttr(ctx, resolved.Object, smb.AttrChange{Attributes: &attributes})
}
