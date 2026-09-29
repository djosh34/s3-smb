package smb2

import (
	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

// validateRequest guards native decoders and confines file IDs to their opening
// session/tree. The underlying VFS handle alone is not an SMB authorization token.
func (t *fileTree) validateRequest(ctx *compoundContext, pkt []byte) NtStatus {
	p := PacketCodec(pkt)
	data := p.Data()
	var id *FileId
	switch p.Command() {
	case SMB2_CLOSE:
		r := CloseRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_FLUSH:
		r := FlushRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_READ:
		r := ReadRequestDecoder(data)
		if r.IsInvalid() || r.Length() > serverMaxReadSize {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_WRITE:
		r := WriteRequestDecoder(data)
		if r.IsInvalid() || r.Length() > serverMaxWriteSize {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_LOCK:
		r := LockRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_QUERY_DIRECTORY:
		r := QueryDirectoryRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_QUERY_INFO:
		r := QueryInfoRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_SET_INFO:
		r := SetInfoRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_CHANGE_NOTIFY:
		r := ChangeNotifyRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
	case SMB2_IOCTL:
		r := IoctlRequestDecoder(data)
		if r.IsInvalid() {
			return STATUS_INVALID_PARAMETER
		}
		id = r.FileId().Decode()
		if IsInvalidFileId(id) {
			return STATUS_SUCCESS
		} // Native share-level FSCTLs have no open.
	default:
		return STATUS_SUCCESS
	}
	if ctx != nil && ctx.fileId != nil {
		id = ctx.fileId
	}
	if t.lookupOpen(id) == nil {
		return STATUS_INVALID_HANDLE
	}
	return STATUS_SUCCESS
}
