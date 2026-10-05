// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func fullSync(file *os.File) error {
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
