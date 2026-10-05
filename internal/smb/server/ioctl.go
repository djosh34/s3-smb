package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func handleIOCTL(_ context.Context, request RequestContext, message wire.Message) (reply, error) {
	ioctl, err := wire.DecodeIOCTLRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	if ioctl.Flags != 1 {
		return reply{status: smb.StatusNotSupported}, nil
	}
	// MS-SMB2 3.3.5.15 exempts these five connection-scoped FSCTLs from open lookup.
	if ioctl.ID == (wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}) {
		switch ioctl.ControlCode {
		case 0x00060194, 0x000601b0, 0x00110018, 0x001401fc, 0x00140204:
			return reply{status: smb.StatusNotSupported}, nil
		}
	}
	open, release, status := useOpen(request, ioctl.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	return reply{status: smb.StatusNotSupported, fileID: wire.FileID(open.ID)}, nil
}
