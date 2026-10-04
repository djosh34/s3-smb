// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"fmt"
	"os"
)

func writeFullSync(path string, fullSync func(uintptr) error) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The caller supplies a new fixture path on the task-owned mounted share.
	if err != nil {
		return err
	}
	if _, err := file.WriteString("F_FULLFSYNC acceptance\n"); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := fullSync(file.Fd()); err != nil {
		return errors.Join(fmt.Errorf("F_FULLFSYNC: %w", err), file.Close())
	}
	return file.Close()
}
