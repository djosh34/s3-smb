//go:build darwin

// SPDX-License-Identifier: AGPL-3.0-only

// Command fullsync creates the file full-sync in a directory on the mounted
// share, writes to it and requires fcntl(F_FULLFSYNC) on it to succeed.
package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fullsync DIRECTORY")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "fullsync:", err)
		os.Exit(1)
	}
}

func run(directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	file, err := root.OpenFile("full-sync", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(err, root.Close())
	}
	_, err = file.WriteString("F_FULLFSYNC acceptance\n")
	if err == nil {
		if _, syncErr := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0); syncErr != nil {
			err = fmt.Errorf("F_FULLFSYNC: %w", syncErr)
		}
	}
	return errors.Join(err, file.Close(), root.Close())
}
