package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func handleFlush(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	flush, err := wire.DecodeFlushRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	var mode smb.SyncMode
	switch flush.Reserved1 {
	case 0:
		mode = smb.SyncData
	case 0xffff:
		mode = smb.SyncFull
	default:
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	open, release, status := useOpen(request, flush.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	result := reply{fileID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}}
	if flushErr := request.Storage.Flush(ctx, open.Handle, mode); flushErr != nil {
		return result, flushErr
	}
	result.body, err = wire.EncodeFlushResponse(wire.EmptyResponse{})
	return result, err
}
