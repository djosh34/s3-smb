package server

import (
	"context"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func handleClose(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	closeRequest, err := wire.DecodeCloseRequest(message)
	if err != nil {
		return reply{}, errors.Join(smb.ErrInvalidParameter, err)
	}
	if closeRequest.Flags & ^uint16(1) != 0 {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	open, release, status := useOpen(request, closeRequest.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	response := wire.CloseResponse{}
	var queryErr error
	if closeRequest.Flags&1 != 0 {
		attr, attrErr := request.Storage.GetAttr(ctx, open.Object)
		if attrErr != nil {
			queryErr = attrErr
		} else {
			response, queryErr = closeResponse(attr)
		}
	}
	release()
	closeErr := closeOpen(context.WithoutCancel(ctx), request, open.ID)
	if errors.Is(closeErr, smb.ErrInvalidHandle) {
		return reply{status: smb.StatusFileClosed}, nil
	}
	if cleanupErr := errors.Join(queryErr, closeErr); cleanupErr != nil {
		return reply{}, cleanupErr
	}
	body, err := wire.EncodeCloseResponse(response)
	return reply{body: body, fileID: wire.FileID(open.ID)}, err
}

func closeResponse(attr smb.Attr) (wire.CloseResponse, error) {
	times, err := createTimes(attr)
	return wire.CloseResponse{
		Flags: 1, Created: times[0], Accessed: times[1], Modified: times[2], Changed: times[3],
		AllocationSize: attr.AllocationSize, Size: attr.Size, Attributes: attr.Attributes,
	}, err
}
