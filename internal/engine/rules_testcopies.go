//go:build testcopies

// SPDX-License-Identifier: AGPL-3.0-only

package engine

import "time"

// Test builds keep 2 copies and make one every 2 minutes, so trash is deleted
// within minutes and a copy lands in the middle of a backup.
const (
	keptCopies = 2
	copyEvery  = 2 * time.Minute
)
