package server

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	fileReadData   uint32 = 0x00000001
	fileWriteData  uint32 = 0x00000002
	fileAppendData uint32 = 0x00000004
	writeThrough   uint32 = 0x00000001
)

func handleRead(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	read, err := wire.DecodeReadRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	open, release, status := useOpen(request, read.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	result := reply{fileID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}}
	if open.GrantedAccess&fileReadData == 0 {
		result.status = smb.StatusAccessDenied
		return result, nil
	}
	if read.Length > smb.MaxReadSize || read.Channel != 0 || read.Offset > math.MaxUint64-uint64(read.Length) {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	if status = request.Opens.CheckIO(open.ID, request.Binding(), read.Offset, uint64(read.Length), false); status != smb.StatusSuccess {
		result.status = status
		return result, nil
	}
	data := make([]byte, read.Length)
	n, err := request.Storage.ReadAt(ctx, open.Handle, data, read.Offset)
	if n < 0 || n > len(data) {
		return result, fmt.Errorf("%w: invalid read count %d", smb.ErrIO, n)
	}
	if err != nil && smb.StatusFromError(err) != smb.StatusEndOfFile {
		return result, err
	}
	if uint64(n) < uint64(read.MinimumCount) || n == 0 && read.Length != 0 {
		result.status = smb.StatusEndOfFile
		return result, nil
	}
	result.body, err = wire.EncodeReadResponse(wire.ReadResponse{Data: data[:n]})
	return result, err
}

func handleWrite(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	write, err := wire.DecodeWriteRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	open, release, status := useOpen(request, write.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	result := reply{fileID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}}
	if open.GrantedAccess&(fileWriteData|fileAppendData) == 0 {
		result.status = smb.StatusAccessDenied
		return result, nil
	}
	if len(write.Data) > int(smb.MaxWriteSize) || write.Channel != 0 || write.Offset == math.MaxUint64 || write.Offset > math.MaxUint64-uint64(len(write.Data)) {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	if open.GrantedAccess&fileWriteData == 0 {
		attr, attrErr := request.Storage.GetAttr(ctx, open.Object)
		if attrErr != nil {
			return result, attrErr
		}
		if write.Offset < attr.Size {
			result.status = smb.StatusAccessDenied
			return result, nil
		}
	}
	if status = request.Opens.CheckIO(open.ID, request.Binding(), write.Offset, uint64(len(write.Data)), true); status != smb.StatusSuccess {
		result.status = status
		return result, nil
	}
	n, err := request.Storage.WriteAt(ctx, open.Handle, write.Data, write.Offset)
	if err != nil {
		return result, err
	}
	if n < 0 || n > math.MaxUint32 || n != len(write.Data) {
		return result, fmt.Errorf("%w: %w", smb.ErrIO, io.ErrShortWrite)
	}
	if write.Flags&writeThrough != 0 {
		if err = request.Storage.Flush(ctx, open.Handle, smb.SyncData); err != nil {
			return result, err
		}
	}
	result.body, err = wire.EncodeWriteResponse(wire.WriteResponse{Count: uint32(n)})
	return result, err
}
