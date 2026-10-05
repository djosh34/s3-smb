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

// handleLock never waits: LOCK is not async eligible and does no storage I/O.
// A conflicting range fails at once with LOCK_NOT_GRANTED, also when the
// client asked to wait, so a single-range lock without FAIL_IMMEDIATELY acts
// as if it had the flag.
func handleLock(_ context.Context, request RequestContext, message wire.Message) (reply, error) {
	if traceNoStreams {
		return reply{status: smb.StatusNotSupported}, nil
	}
	body, err := wire.DecodeLockRequest(message)
	if err != nil {
		return reply{status: smb.StatusInvalidParameter}, nil
	}
	open, release, status := useOpen(request, body.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer release()
	result := reply{fileID: wire.FileID(open.ID)}
	ranges, unlock, status := lockRanges(body.Elements)
	if status != smb.StatusSuccess {
		result.status = status
		return result, nil
	}
	if open.Kind == smb.KindDirectory {
		// MS-FSA 2.1.5.7 forbids byte-range locks on directories.
		result.status = smb.StatusInvalidParameter
		return result, nil
	}
	if result.status = request.Opens.Lock(open.ID, request.Binding(), ranges, unlock); result.status != smb.StatusSuccess {
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
		// MS-SMB2 3.3.5.14.2 requires immediate failure for multi-range locks.
		if !unlock && len(elements) > 1 && flags&lockFailImmediately == 0 {
			return nil, false, smb.StatusInvalidParameter
		}
		ranges[index] = state.Range{Offset: element.Offset, Length: element.Length, Exclusive: flags&lockExclusive != 0}
	}
	return ranges, unlock, smb.StatusSuccess
}
