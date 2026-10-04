//go:build !darwin

// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"fmt"
)

func fullSync(_ uintptr) error {
	return fmt.Errorf("F_FULLFSYNC requires Darwin: %w", errors.ErrUnsupported)
}
