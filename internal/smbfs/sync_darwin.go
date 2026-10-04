package smbfs

import (
	"os"

	"golang.org/x/sys/unix"
)

func syncFile(file *os.File, full bool) error {
	if !full {
		return file.Sync()
	}
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
