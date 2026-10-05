//go:build !darwin

// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"testing"
)

func TestFullSyncUnsupported(t *testing.T) {
	if err := run(t.TempDir()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("run = %v, want ErrUnsupported", err)
	}
}
