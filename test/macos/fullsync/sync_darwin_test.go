// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFullSyncFailure(t *testing.T) {
	if err := fullSync(^uintptr(0)); !errors.Is(err, unix.EBADF) {
		t.Fatal("full sync must preserve the fcntl error", err)
	}
}
