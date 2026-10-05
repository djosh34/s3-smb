package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// MS-FSA 2.1.5.14.2 limits client-settable attributes to these seven bits.
const clientSettableFileAttributes uint32 = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000020 | 0x00000100 | 0x00001000 | 0x00002000

func normalizeFileAttributes(attributes uint32, directory bool) uint32 {
	attributes &= clientSettableFileAttributes
	if directory {
		return attributes | 0x10 // FILE_ATTRIBUTE_DIRECTORY is determined by storage.
	}
	if attributes == 0 {
		return 0x80 // FILE_ATTRIBUTE_NORMAL.
	}
	return attributes
}

func setCreateAttributes(ctx context.Context, storage smb.Storage, create wire.CreateRequest, resolved smb.Resolved, action uint32) error {
	directory := resolved.Attr.Kind == smb.KindDirectory
	// CREATE attributes describe the unnamed file, never a selected named stream.
	if resolved.Object.Stream != "" || action == 1 || action != 0 && create.FileAttributes == 0 && directory {
		return nil
	}
	attributes := create.FileAttributes
	if action == 3 {
		attributes |= resolved.Attr.Attributes
	}
	if !directory {
		attributes |= 0x20 // MS-FSA sets FILE_ATTRIBUTE_ARCHIVE on new or replaced data files.
	}
	attributes = normalizeFileAttributes(attributes, directory)
	return storage.SetAttr(ctx, resolved.Object, smb.AttrChange{Attributes: &attributes})
}
