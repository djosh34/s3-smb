//go:build !darwin

// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"os"
)

func fullSync(*os.File) error { return errors.ErrUnsupported }
