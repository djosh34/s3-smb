package server

import (
	"context"
	"encoding/binary"
	"strings"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func handleQueryInfo(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	query, err := wire.DecodeQueryInfoRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	id, status := request.FileID(query.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	open, release, status := useOpen(request, id)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	result := reply{fileID: id}
	if query.OutputLength > smb.MaxTransactSize {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	var data []byte
	switch query.InfoType {
	case wire.InfoFile:
		data, result.status = queryFileInfo(ctx, request, open, wire.FileInfoClass(query.InfoClass), query.OutputLength)
	case wire.InfoFilesystem:
		// The filesystem worker wires queryFilesystemInfo here in #109.
		result.status = smb.StatusInvalidInfoClass
	case wire.InfoSecurity:
		// The storage contract and #169 promise no ACL queries or ACL fidelity.
		result.status = smb.StatusNotSupported
	default:
		result.status = smb.StatusNotSupported
	}
	if result.status != smb.StatusSuccess && result.status != smb.StatusBufferOverflow {
		return result, nil
	}
	result.body, err = wire.EncodeQueryInfoResponse(wire.QueryResponse{Data: data})
	if err != nil {
		result.status = smb.StatusInternalError
	}
	return result, nil
}

// Fixed portions are from MS-FSCC 2.4. MS-SMB2 3.3.5.20.1 permits a
// truncated variable portion, but requires the whole fixed portion.
func fileInfoFixedSize(class wire.FileInfoClass) uint32 {
	switch class {
	case wire.ClassFileBasic:
		return 40
	case wire.ClassFileStandard, wire.ClassFileID, wire.ClassFileStream:
		return 24
	case wire.ClassFileInternal, wire.ClassFilePosition, wire.ClassFileAttributeTag:
		return 8
	case wire.ClassFileEA, wire.ClassFileAccess, wire.ClassFileMode, wire.ClassFileAlignment, wire.ClassFileName:
		return 4
	case wire.ClassFileAll:
		return 100
	case wire.ClassFileNetworkOpen:
		return 56
	case wire.ClassFileRename, wire.ClassFileDisposition, wire.ClassFileAllocation, wire.ClassFileEndOfFile:
		return 0
	default:
		return 0
	}
}

func queryFileInfo(ctx context.Context, request RequestContext, open state.Open, class wire.FileInfoClass, outputLength uint32) ([]byte, smb.Status) {
	fixed := fileInfoFixedSize(class)
	if fixed == 0 {
		return nil, smb.StatusInvalidInfoClass
	}
	if outputLength < fixed {
		return nil, smb.StatusInfoLengthMismatch
	}
	attr, err := request.Storage.GetAttr(ctx, open.Object)
	if err != nil {
		return nil, smb.StatusFromError(err)
	}
	data, err := encodeFileInfo(ctx, request, open, attr, class)
	if err != nil {
		return nil, smb.StatusFromError(err)
	}
	if uint64(len(data)) > uint64(outputLength) {
		return data[:outputLength], smb.StatusBufferOverflow
	}
	return data, smb.StatusSuccess
}

func queryBasicInfo(attr smb.Attr) (wire.FileBasicInformation, error) {
	var info wire.FileBasicInformation
	var err error
	info.Created, err = wire.EncodeFiletime(attr.Created)
	if err != nil {
		return info, err
	}
	info.Accessed, err = wire.EncodeFiletime(attr.Accessed)
	if err != nil {
		return info, err
	}
	info.Modified, err = wire.EncodeFiletime(attr.Modified)
	if err != nil {
		return info, err
	}
	info.Changed, err = wire.EncodeFiletime(attr.Changed)
	info.Attributes = attr.Attributes
	return info, err
}

func queryStandardInfo(request RequestContext, open state.Open, attr smb.Attr) wire.FileStandardInformation {
	return wire.FileStandardInformation{
		AllocationSize: attr.AllocationSize, EndOfFile: attr.Size, Links: 1,
		DeletePending: request.Opens.DeletePending(open.Object), Directory: attr.Kind == smb.KindDirectory,
	}
}

func queryNameInfo(ctx context.Context, request RequestContext, open state.Open) (wire.FileNameInformation, error) {
	path, err := request.Storage.PathOf(ctx, open.Object.Inode)
	if err != nil {
		return wire.FileNameInformation{}, err
	}
	name := "\\" + strings.ReplaceAll(path, "/", "\\")
	if open.Object.Stream != "" {
		name += ":" + open.Object.Stream + ":$DATA"
	}
	return wire.FileNameInformation{Name: name}, nil
}

func encodeFileInfo(ctx context.Context, request RequestContext, open state.Open, attr smb.Attr, class wire.FileInfoClass) ([]byte, error) {
	switch class {
	case wire.ClassFileBasic:
		basic, err := queryBasicInfo(attr)
		if err != nil {
			return nil, err
		}
		return wire.EncodeFileBasicInformation(basic)
	case wire.ClassFileStandard:
		return wire.EncodeFileStandardInformation(queryStandardInfo(request, open, attr))
	case wire.ClassFileInternal:
		return wire.EncodeFileInternalInformation(wire.FileInternalInformation{Index: uint64(attr.Inode)})
	case wire.ClassFileEA:
		// Named xattrs are exposed as streams, not Windows extended attributes.
		return wire.EncodeFileEAInformation(wire.FileEAInformation{})
	case wire.ClassFileAccess:
		return wire.EncodeFileAccessInformation(wire.FileAccessInformation{Access: open.GrantedAccess})
	case wire.ClassFilePosition:
		// SMB offsets are explicit; MS-SMB2 recommends a zero current position.
		return wire.EncodeFilePositionInformation(wire.FilePositionInformation{})
	case wire.ClassFileMode:
		return wire.EncodeFileModeInformation(wire.FileModeInformation{})
	case wire.ClassFileAlignment:
		return wire.EncodeFileAlignmentInformation(wire.FileAlignmentInformation{})
	case wire.ClassFileName:
		name, err := queryNameInfo(ctx, request, open)
		if err != nil {
			return nil, err
		}
		return wire.EncodeFileNameInformation(name)
	case wire.ClassFileAll:
		return encodeAllFileInfo(ctx, request, open, attr)
	case wire.ClassFileNetworkOpen:
		basic, err := queryBasicInfo(attr)
		if err != nil {
			return nil, err
		}
		return wire.EncodeFileNetworkOpenInformation(wire.FileNetworkOpenInformation{
			Created: basic.Created, Accessed: basic.Accessed, Modified: basic.Modified, Changed: basic.Changed,
			AllocationSize: attr.AllocationSize, EndOfFile: attr.Size, Attributes: attr.Attributes,
		})
	case wire.ClassFileAttributeTag:
		return wire.EncodeFileAttributeTagInformation(wire.FileAttributeTagInformation{Attributes: attr.Attributes})
	case wire.ClassFileStream:
		return encodeStreamInfo(ctx, request, open, attr)
	case wire.ClassFileID:
		space, err := request.Storage.StatFS(ctx)
		if err != nil {
			return nil, err
		}
		info := wire.FileIDInformation{VolumeSerial: space.VolumeID}
		binary.LittleEndian.PutUint64(info.ID[:8], uint64(attr.Inode))
		return wire.EncodeFileIDInformation(info)
	case wire.ClassFileRename, wire.ClassFileDisposition, wire.ClassFileAllocation, wire.ClassFileEndOfFile:
		return nil, smb.ErrNotSupported
	default:
		return nil, smb.ErrNotSupported
	}
}

func encodeAllFileInfo(ctx context.Context, request RequestContext, open state.Open, attr smb.Attr) ([]byte, error) {
	basic, err := queryBasicInfo(attr)
	if err != nil {
		return nil, err
	}
	name, err := queryNameInfo(ctx, request, open)
	if err != nil {
		return nil, err
	}
	return wire.EncodeFileAllInformation(wire.FileAllInformation{
		Basic: basic, Standard: queryStandardInfo(request, open, attr),
		Internal: wire.FileInternalInformation{Index: uint64(attr.Inode)},
		Access:   wire.FileAccessInformation{Access: open.GrantedAccess}, Name: name,
	})
}

func encodeStreamInfo(ctx context.Context, request RequestContext, open state.Open, attr smb.Attr) ([]byte, error) {
	if attr.Kind == smb.KindDirectory {
		return wire.EncodeFileStreamInformation(wire.FileStreamInformation{})
	}
	streams, err := request.Storage.Streams(ctx, open.Object.Inode)
	if err != nil {
		return nil, err
	}
	// GetAttr always selects the open's object. The unnamed entry needs its own
	// attributes only when this query was made through a named-stream open.
	base := attr
	if open.Object.Stream != "" {
		base, err = request.Storage.GetAttr(ctx, smb.ObjectKey{Inode: open.Object.Inode})
		if err != nil {
			return nil, err
		}
	}
	entries := []wire.FileStreamEntry{{Name: "::$DATA", Size: base.Size, AllocationSize: base.AllocationSize}}
	for _, stream := range streams {
		entries = append(entries, wire.FileStreamEntry{Name: ":" + stream.Name + ":$DATA", Size: stream.Size, AllocationSize: stream.AllocationSize})
	}
	return wire.EncodeFileStreamInformation(wire.FileStreamInformation{Entries: entries})
}
