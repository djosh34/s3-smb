package server

import (
	"context"
	"encoding/binary"
	"math"
	"path"
	"strings"
	"unicode/utf16"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	directoryRestart = 0x01
	directorySingle  = 0x02
	directoryIndex   = 0x04
	directoryReopen  = 0x10
	directoryPage    = 64
)

func handleQueryDirectory(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	query, err := wire.DecodeQueryDirectoryRequest(message)
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
	attr, err := request.Storage.GetAttr(ctx, open.Object)
	if err != nil {
		return result, err
	}
	if attr.Kind != smb.KindDirectory {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	if open.GrantedAccess&1 == 0 { // FILE_LIST_DIRECTORY.
		result.status = smb.StatusAccessDenied
		return result, nil
	}
	if status = directoryClassStatus(query.InfoClass); status != smb.StatusSuccess {
		result.status = status
		return result, nil
	}
	if query.OutputLength > smb.MaxTransactSize || query.Flags & ^uint8(directoryRestart|directorySingle|directoryIndex|directoryReopen) != 0 {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	if len(utf16.Encode([]rune(query.Pattern))) > 255 {
		result.status = smb.StatusObjectNameInvalid
		return result, nil
	}
	cursor := directoryCursor(open.Directory, query)
	// MS-SMB2 3.3.5.18 permits ignoring INDEX_SPECIFIED. Storage cookies are
	// opaque, not 32-bit byte indexes. All returned FileIndex fields are zero.
	data, cursor, status, err := scanDirectory(ctx, request.Storage, open.Object.Inode, cursor, query)
	if err != nil {
		return result, err
	}
	if saved := request.Opens.SetDirectory(open.ID, request.Binding(), cursor); saved != smb.StatusSuccess {
		result.status = saved
		return result, nil
	}
	result.status = status
	if status == smb.StatusSuccess {
		result.body, err = wire.EncodeQueryDirectoryResponse(wire.QueryResponse{Data: data})
	}
	return result, err
}

func directoryClassStatus(class wire.DirectoryInfoClass) smb.Status {
	switch class {
	case wire.ClassDirectory, wire.ClassDirectoryFull, wire.ClassDirectoryBoth, wire.ClassDirectoryNames, wire.ClassDirectoryIDBoth, wire.ClassDirectoryIDFull:
		return smb.StatusSuccess
	case 0x3c, 0x4e, 0x4f, 0x50, 0x51: // Extended ID layouts from MS-SMB2 2.2.33.
		return smb.StatusNotSupported
	default:
		return smb.StatusInvalidInfoClass
	}
}

func directoryCursor(saved state.DirectoryCursor, query wire.QueryDirectoryRequest) state.DirectoryCursor {
	if query.Flags&(directoryRestart|directoryReopen) != 0 {
		saved.Cookie, saved.DotEntries, saved.Started = 0, 0, false
	}
	if saved.Pattern == "" || query.Flags&directoryReopen != 0 {
		saved.Pattern = query.Pattern
	}
	if saved.Pattern == "" {
		saved.Pattern = "*"
	}
	return saved
}

func scanDirectory(ctx context.Context, storage smb.Storage, inode smb.Inode, cursor state.DirectoryCursor, query wire.QueryDirectoryRequest) ([]byte, state.DirectoryCursor, smb.Status, error) {
	dots, err := directoryDots(ctx, storage, inode, cursor)
	if err != nil {
		return nil, cursor, 0, err
	}
	if len(dots) == 0 {
		cursor.DotEntries = 2
	}
	scan := directoryScan{cursor: cursor, query: query}
	first := !cursor.Started
	for {
		entries := dots
		synthetic := len(dots) > 0
		if !synthetic {
			entries, err = storage.ReadDir(ctx, inode, scan.cursor.Cookie, directoryPage)
			if err != nil {
				return nil, scan.cursor, 0, err
			}
		}
		if len(entries) == 0 {
			scan.cursor.Started = true
			status := smb.StatusSuccess
			if len(scan.data) == 0 {
				status = smb.StatusNoMoreFiles
				if first {
					status = smb.StatusNoSuchFile
				}
			}
			return scan.data, scan.cursor, status, nil
		}
		stop, status, pageErr := scan.page(entries, synthetic)
		if stop || pageErr != nil {
			return scan.data, scan.cursor, status, pageErr
		}
		dots = nil
	}
}

type directoryScan struct {
	data   []byte
	cursor state.DirectoryCursor
	query  wire.QueryDirectoryRequest
	last   int
}

func (scan *directoryScan) page(entries []smb.DirEntry, synthetic bool) (bool, smb.Status, error) {
	for _, entry := range entries {
		if matchPattern(scan.cursor.Pattern, entry.Name) {
			encoded, err := encodeDirectoryEntry(scan.query.InfoClass, entry)
			if err != nil {
				return true, 0, err
			}
			if !scan.appendEntry(encoded) {
				status := smb.StatusSuccess
				if len(scan.data) == 0 {
					status = smb.StatusBufferTooSmall
				}
				return true, status, nil
			}
			scan.cursor.Started = true
		}
		if synthetic {
			scan.cursor.DotEntries++
		} else {
			scan.cursor.Cookie = entry.Next
		}
		if len(scan.data) > 0 && scan.query.Flags&directorySingle != 0 {
			return true, smb.StatusSuccess, nil
		}
	}
	return false, smb.StatusSuccess, nil
}

func (scan *directoryScan) appendEntry(encoded []byte) bool {
	padding := (8 - len(scan.data)%8) % 8
	if len(scan.data)+padding+len(encoded) > int(scan.query.OutputLength) {
		return false
	}
	if len(scan.data) > 0 {
		next := len(scan.data) + padding - scan.last
		if next < 0 || next > math.MaxUint32 {
			return false
		}
		scan.data = append(scan.data, make([]byte, padding)...)
		binary.LittleEndian.PutUint32(scan.data[scan.last:], uint32(next))
	}
	scan.last = len(scan.data)
	scan.data = append(scan.data, encoded...)
	return true
}

func directoryDots(ctx context.Context, storage smb.Storage, inode smb.Inode, cursor state.DirectoryCursor) ([]smb.DirEntry, error) {
	if cursor.DotEntries >= 2 || !matchPattern(cursor.Pattern, ".") && !matchPattern(cursor.Pattern, "..") {
		return nil, nil
	}
	name, err := storage.PathOf(ctx, inode)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, nil
	}
	parentPath := path.Dir(strings.ReplaceAll(name, "\\", "/"))
	if parentPath == "." {
		parentPath = ""
	}
	parent, err := storage.Lookup(ctx, parentPath)
	if err != nil {
		return nil, err
	}
	if !parent.Exists {
		return nil, smb.ErrPathNotFound
	}
	var entries []smb.DirEntry
	if cursor.DotEntries == 0 {
		attr, attrErr := storage.GetAttr(ctx, smb.ObjectKey{Inode: inode})
		if attrErr != nil {
			return nil, attrErr
		}
		entries = append(entries, smb.DirEntry{Name: ".", Attr: attr})
	}
	return append(entries, smb.DirEntry{Name: "..", Attr: parent.Attr}), nil
}

func encodeDirectoryEntry(class wire.DirectoryInfoClass, entry smb.DirEntry) ([]byte, error) {
	metadata := wire.DirectoryMetadata{EndOfFile: entry.Attr.Size, AllocationSize: entry.Attr.AllocationSize, FileID: uint64(entry.Attr.Inode)}
	metadata.Basic.Attributes = entry.Attr.Attributes
	if class != wire.ClassDirectoryNames {
		var err error
		metadata.Basic.Created, err = wire.EncodeFiletime(entry.Attr.Created)
		if err != nil {
			return nil, err
		}
		metadata.Basic.Accessed, err = wire.EncodeFiletime(entry.Attr.Accessed)
		if err != nil {
			return nil, err
		}
		metadata.Basic.Modified, err = wire.EncodeFiletime(entry.Attr.Modified)
		if err != nil {
			return nil, err
		}
		metadata.Basic.Changed, err = wire.EncodeFiletime(entry.Attr.Changed)
		if err != nil {
			return nil, err
		}
	}
	entries := []wire.DirectoryEntry{{Name: entry.Name, Metadata: metadata}}
	switch uint8(class) {
	case uint8(wire.ClassDirectory):
		return wire.EncodeDirectoryEntries(entries)
	case uint8(wire.ClassDirectoryFull):
		return wire.EncodeDirectoryFullEntries(entries)
	case uint8(wire.ClassDirectoryBoth):
		return wire.EncodeDirectoryBothEntries(entries)
	case uint8(wire.ClassDirectoryNames):
		return wire.EncodeDirectoryNamesEntries(entries)
	case uint8(wire.ClassDirectoryIDBoth):
		return wire.EncodeDirectoryIDBothEntries([]wire.DirectoryIDBothEntry{{Name: entry.Name, Metadata: metadata}})
	case uint8(wire.ClassDirectoryIDFull):
		return wire.EncodeDirectoryIDFullEntries([]wire.DirectoryIDFullEntry{{Name: entry.Name, Metadata: metadata}})
	default:
		return nil, smb.ErrInvalidParameter
	}
}
