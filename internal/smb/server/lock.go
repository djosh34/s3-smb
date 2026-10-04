package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	lockShared          uint32 = 0x01
	lockExclusive       uint32 = 0x02
	lockUnlock          uint32 = 0x04
	lockFailImmediately uint32 = 0x10
)

func handleLock(_ context.Context, request RequestContext, message wire.Message) (reply, error) {
	body, err := wire.DecodeLockRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	id, status := request.FileID(body.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	result := reply{fileID: id}
	open, release, status := useOpen(request, id)
	if status != smb.StatusSuccess {
		result.status = status
		return result, nil
	}
	defer release()
	ranges, unlock, status := lockRanges(body.Elements)
	if status != smb.StatusSuccess {
		result.status = status
		return result, nil
	}
	// The table keys ranges by the open's complete ObjectKey and applies the
	// vector atomically. FAIL_IMMEDIATELY never changes our non-blocking policy.
	result.status = request.Opens.Lock(open.ID, request.Binding(), ranges, unlock)
	if result.status != smb.StatusSuccess {
		return result, nil
	}
	result.body, err = wire.EncodeLockResponse(wire.EmptyResponse{})
	return result, err
}

func lockRanges(elements []wire.LockElement) ([]state.Range, bool, smb.Status) {
	if len(elements) == 0 {
		return nil, false, smb.StatusInvalidParameter
	}
	unlock := elements[0].Flags == lockUnlock
	ranges := make([]state.Range, len(elements))
	for index, element := range elements {
		flags := element.Flags
		if unlock {
			if flags != lockUnlock {
				return nil, false, smb.StatusInvalidParameter
			}
		} else if flags != lockShared && flags != lockExclusive && flags != lockShared|lockFailImmediately && flags != lockExclusive|lockFailImmediately {
			return nil, false, smb.StatusInvalidParameter
		}
		ranges[index] = state.Range{Offset: element.Offset, Length: element.Length, Exclusive: flags&lockExclusive != 0}
	}
	return ranges, unlock, smb.StatusSuccess
}
