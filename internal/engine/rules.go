//go:build !testcopies

// SPDX-License-Identifier: AGPL-3.0-only

package engine

import "time"

// The copy rules: keep the 4 newest copies and make one every 15 minutes.
// The testcopies build tag shortens them for the Mac thinning and rollback
// runs.
const (
	keptCopies = 4
	copyEvery  = 15 * time.Minute
)
