package server

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const writeThrough uint32 = 0x00000001

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
	result := reply{fileID: wire.FileID(open.ID)}
	// Execute access reads too (MS-SMB2 3.3.5.12).
	if open.GrantedAccess&(fileReadData|fileExecute) == 0 {
		result.status = smb.StatusAccessDenied
		return result, nil
	}
	if read.Length > smb.MaxReadSize || read.Channel != 0 || read.Offset > math.MaxInt64-uint64(read.Length) {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	if open.Kind == smb.KindDirectory {
		result.status = smb.StatusInvalidDeviceRequest
		return result, nil
	}
	if read.Length == 0 {
		if read.MinimumCount != 0 {
			result.status = smb.StatusEndOfFile
			return result, nil
		}
		result.body, err = wire.EncodeReadResponse(wire.ReadResponse{})
		return result, err
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
	result := reply{fileID: wire.FileID(open.ID)}
	if open.GrantedAccess&(fileWriteData|fileAppendData) == 0 {
		result.status = smb.StatusAccessDenied
		return result, nil
	}
	length := uint64(len(write.Data))
	if length > uint64(smb.MaxWriteSize) || write.Channel != 0 || write.Offset == math.MaxUint64 || write.Offset > math.MaxUint64-length {
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	n, err := request.Storage.WriteAt(ctx, open.Handle, write.Data, write.Offset)
	if err != nil {
		return result, err
	}
	if n != len(write.Data) {
		return result, fmt.Errorf("%w: %w", smb.ErrIO, io.ErrShortWrite)
	}
	if write.Flags&writeThrough != 0 || open.WriteThrough {
		if err = request.Storage.Flush(ctx, open.Handle, smb.SyncData); err != nil {
			return result, err
		}
	}
	result.body, err = wire.EncodeWriteResponse(wire.WriteResponse{Count: uint32(length)})
	return result, err
}
