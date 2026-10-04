//go:build !darwin

// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullSyncUnsupported(t *testing.T) {
	err := writeFullSync(filepath.Join(t.TempDir(), "file"), fullSync)
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "F_FULLFSYNC requires Darwin") {
		t.Fatal("full sync must fail explicitly off Darwin", err)
	}
}
