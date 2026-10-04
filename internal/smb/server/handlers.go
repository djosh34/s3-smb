package server

import "github.com/djosh34/s3-smb/internal/smb/wire"

// commandHandlers maps each file command to its handler, in command order.
func commandHandlers() map[wire.Command]handler {
	return map[wire.Command]handler{
		wire.Create:         handleCreate,
		wire.Close:          handleClose,
		wire.Flush:          handleFlush,
		wire.Read:           handleRead,
		wire.Write:          handleWrite,
		wire.Lock:           handleLock,
		wire.IOCTL:          handleIOCTL,
		wire.Echo:           handleEcho,
		wire.QueryDirectory: handleQueryDirectory,
		wire.QueryInfo:      handleQueryInfo,
		wire.SetInfo:        handleSetInfo,
		wire.OplockBreak:    handleOplockBreak,
	}
}
