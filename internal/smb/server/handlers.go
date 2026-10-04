package server

import "github.com/djosh34/s3-smb/internal/smb/wire"

// Each area adds only its own handler line, in command order.
func commandHandlers() map[wire.Command]handler {
	return map[wire.Command]handler{
		wire.Create:         handleCreate,
		wire.Close:          handleClose,
		wire.Flush:          handleFlush,
		wire.Read:           handleRead,
		wire.Write:          handleWrite,
		wire.Echo:           handleEcho,
		wire.QueryDirectory: handleQueryDirectory,
		wire.QueryInfo:      handleQueryInfo,
		wire.OplockBreak:    handleOplockBreak,
	}
}
