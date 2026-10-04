//go:build darwin

// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"log"
	"os"

	"golang.org/x/sys/unix"
)

func main() {
	var err error
	if len(os.Args) != 2 {
		err = errors.New("usage: fullsync <new file on the mounted share>")
	} else {
		err = writeFullSync(os.Args[1], func(fd uintptr) error {
			_, syncErr := unix.FcntlInt(fd, unix.F_FULLFSYNC, 0)
			return syncErr
		})
	}
	if err != nil {
		log.New(os.Stderr, "fullsync: ", 0).Print(err)
		os.Exit(1)
	}
}
