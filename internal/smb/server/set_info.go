package server

import (
	"context"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Matches smbfs allocation accounting and the filesystem information replies.
const setInfoAllocationUnit uint64 = 4096

func handleSetInfo(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	info, err := wire.DecodeSetInfoRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	id, status := request.FileID(info.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	open, release, status := useOpen(request, id)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	result := reply{fileID: id}
	if info.InfoType != wire.InfoFile {
		result.status = smb.StatusNotSupported
		return result, nil
	}
	switch info.InfoClass {
	case uint8(wire.ClassFileBasic):
		result.status = setBasicInfo(ctx, request, open, info.Input)
	case uint8(wire.ClassFileEndOfFile):
		result.status = setEndOfFileInfo(ctx, request, open, info.Input)
	case uint8(wire.ClassFileAllocation):
		result.status = setAllocationInfo(ctx, request, open, info.Input)
	case uint8(wire.ClassFileRename):
		result.status = setRenameInfo(ctx, request, open, info.Input)
	case uint8(wire.ClassFileDisposition):
		result.status = setDispositionInfo(ctx, request, open, info.Input)
	default:
		// Hard links are not supported.
		result.status = smb.StatusNotSupported
	}
	if result.status != smb.StatusSuccess {
		return result, nil
	}
	result.body, err = wire.EncodeSetInfoResponse(wire.EmptyResponse{})
	return result, err
}

func setBasicInfo(ctx context.Context, request RequestContext, open state.Open, buffer []byte) smb.Status {
	if open.GrantedAccess&0x00000100 == 0 { // FILE_WRITE_ATTRIBUTES.
		return smb.StatusAccessDenied
	}
	info, err := wire.DecodeFileBasicInformation(buffer)
	if err != nil {
		return smb.StatusInfoLengthMismatch
	}
	var change smb.AttrChange
	for _, field := range []struct {
		target **time.Time
		value  wire.Filetime
	}{
		{&change.Created, info.Created},
		{&change.Accessed, info.Accessed},
		{&change.Modified, info.Modified},
		{&change.Changed, info.Changed},
	} {
		update, decodeErr := wire.DecodeTimeUpdate(field.value)
		if decodeErr != nil || update.Action == wire.TimeSet && field.value >= 1<<63 {
			return smb.StatusInvalidParameter
		}
		if update.Action == wire.TimeSet {
			*field.target = &update.Time
		}
	}
	if info.Attributes != 0 {
		attr, attrErr := request.Storage.GetAttr(ctx, open.Object)
		if attrErr != nil {
			return setInfoStorageStatus(ctx, request, attrErr)
		}
		if attr.Kind == smb.KindFile && info.Attributes&0x10 != 0 || attr.Kind == smb.KindDirectory && info.Attributes&0x100 != 0 {
			return smb.StatusInvalidParameter
		}
		attributes := attr.Attributes&^clientSettableFileAttributes | info.Attributes&clientSettableFileAttributes
		change.Attributes = &attributes
	}
	return setInfoStorageStatus(ctx, request, request.Storage.SetAttr(ctx, open.Object, change))
}

func setEndOfFileInfo(ctx context.Context, request RequestContext, open state.Open, buffer []byte) smb.Status {
	if open.GrantedAccess&0x00000002 == 0 { // FILE_WRITE_DATA, not FILE_APPEND_DATA.
		return smb.StatusAccessDenied
	}
	info, err := wire.DecodeFileEndOfFileInformation(buffer)
	if err != nil {
		return smb.StatusInfoLengthMismatch
	}
	if info.EndOfFile >= 1<<63 {
		return smb.StatusInvalidParameter
	}
	attr, err := request.Storage.GetAttr(ctx, open.Object)
	if err != nil {
		return setInfoStorageStatus(ctx, request, err)
	}
	if attr.Kind == smb.KindDirectory {
		return smb.StatusInvalidParameter
	}
	return setInfoStorageStatus(ctx, request, request.Storage.SetAttr(ctx, open.Object, smb.AttrChange{Size: &info.EndOfFile}))
}

func setAllocationInfo(ctx context.Context, request RequestContext, open state.Open, buffer []byte) smb.Status {
	if open.GrantedAccess&0x00000002 == 0 { // FILE_WRITE_DATA.
		return smb.StatusAccessDenied
	}
	info, err := wire.DecodeFileAllocationInformation(buffer)
	if err != nil {
		return smb.StatusInfoLengthMismatch
	}
	// The rounded allocation must still fit the signed protocol size field.
	if info.AllocationSize > 1<<63-setInfoAllocationUnit {
		return smb.StatusInvalidParameter
	}
	allocation := (info.AllocationSize + setInfoAllocationUnit - 1) / setInfoAllocationUnit * setInfoAllocationUnit
	attr, err := request.Storage.GetAttr(ctx, open.Object)
	if err != nil {
		return setInfoStorageStatus(ctx, request, err)
	}
	if attr.Kind == smb.KindDirectory {
		return smb.StatusInvalidParameter
	}
	if allocation >= attr.Size {
		return smb.StatusSuccess
	}
	return setInfoStorageStatus(ctx, request, request.Storage.SetAttr(ctx, open.Object, smb.AttrChange{Size: &allocation}))
}

func setInfoStorageStatus(ctx context.Context, request RequestContext, err error) smb.Status {
	if err != nil {
		request.server.options.Logger.ErrorContext(ctx, "SET_INFO storage failed", "error", err)
	}
	return smb.StatusFromError(err)
}
