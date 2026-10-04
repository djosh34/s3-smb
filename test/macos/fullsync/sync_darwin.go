// SPDX-License-Identifier: AGPL-3.0-only
package main

import "golang.org/x/sys/unix"

func fullSync(fd uintptr) error {
	_, err := unix.FcntlInt(fd, unix.F_FULLFSYNC, 0)
	return err
}
