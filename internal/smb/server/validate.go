package server

import (
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// validateCompound decodes all known bodies and checks charges before dispatch.
// A bad body rejects the compound as a whole, without running a prefix handler.
func validateCompound(messages []wire.Message) error {
	for _, message := range messages {
		header := message.Header
		if header.Flags&wire.FlagResponse != 0 || header.Flags&wire.FlagAsync != 0 && header.Command != wire.Cancel {
			return errors.New("request uses a response header")
		}
		if (header.Command == wire.Negotiate || header.Command == wire.SessionSetup) && len(messages) != 1 {
			return errors.New("NEGOTIATE and SESSION_SETUP cannot be compounded")
		}
		size, err := requestSize(message)
		if err != nil {
			return err
		}
		required := max(uint64(1), (size+uint64(smb.CreditUnit)-1)/uint64(smb.CreditUnit))
		if uint64(creditCharge(header)) < required {
			return errors.New("credit charge is smaller than request size")
		}
	}
	return nil
}

// requestSize returns the larger input or expected output size for commands
// with multi-credit semantics. Other commands still get their typed validation.
func requestSize(message wire.Message) (uint64, error) {
	switch message.Header.Command {
	case wire.Negotiate:
		_, err := wire.DecodeNegotiateRequest(message)
		return 0, err
	case wire.SessionSetup:
		_, err := wire.DecodeSessionSetupRequest(message)
		return 0, err
	case wire.Logoff:
		_, err := wire.DecodeLogoffRequest(message)
		return 0, err
	case wire.TreeConnect:
		_, err := wire.DecodeTreeConnectRequest(message)
		return 0, err
	case wire.TreeDisconnect:
		_, err := wire.DecodeTreeDisconnectRequest(message)
		return 0, err
	case wire.Create:
		_, err := wire.DecodeCreateRequest(message)
		return 0, err
	case wire.Close:
		_, err := wire.DecodeCloseRequest(message)
		return 0, err
	case wire.Flush:
		_, err := wire.DecodeFlushRequest(message)
		return 0, err
	case wire.Read:
		request, err := wire.DecodeReadRequest(message)
		return uint64(request.Length), err
	case wire.Write:
		request, err := wire.DecodeWriteRequest(message)
		return uint64(len(request.Data)), err
	case wire.Lock:
		_, err := wire.DecodeLockRequest(message)
		return 0, err
	case wire.IOCTL:
		request, err := wire.DecodeIOCTLRequest(message)
		return max(uint64(len(request.Input)), uint64(request.MaxOutput)), err
	case wire.Cancel:
		_, err := wire.DecodeCancelRequest(message)
		return 0, err
	case wire.Echo:
		_, err := wire.DecodeEchoRequest(message)
		return 0, err
	case wire.QueryDirectory:
		request, err := wire.DecodeQueryDirectoryRequest(message)
		return uint64(request.OutputLength), err
	case wire.ChangeNotify:
		_, err := wire.DecodeChangeNotifyRequest(message)
		return 0, err
	case wire.QueryInfo:
		request, err := wire.DecodeQueryInfoRequest(message)
		return max(uint64(len(request.Input)), uint64(request.OutputLength)), err
	case wire.SetInfo:
		_, err := wire.DecodeSetInfoRequest(message)
		return 0, err
	case wire.OplockBreak:
		return 0, validateOplockBreak(message)
	default:
		return 0, nil
	}
}
