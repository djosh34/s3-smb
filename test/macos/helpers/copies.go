// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"encoding/json"
	"strings"
)

// NewestCopy returns the newest database copy among bucket keys, or "" when
// there is none. Copy names are zero-padded, so the highest name is newest.
func NewestCopy(objects map[string]int64) string {
	newest := ""
	for key := range objects {
		if strings.HasPrefix(key, "db/") && key > newest {
			newest = key
		}
	}
	return newest
}

// RestoredCopy returns the database copy that a start restored, from the
// application's JSON log, or "" when it restored none.
func RestoredCopy(log string) string {
	restored := ""
	for line := range strings.Lines(log) {
		if copy, ok := logCopy(line, "restoring the newest database copy"); ok {
			restored = copy
		}
	}
	return restored
}

// LandedCopies returns the database copies that the application's JSON log
// says landed, in order.
func LandedCopies(log string) []string {
	var landed []string
	for line := range strings.Lines(log) {
		if copy, ok := logCopy(line, "database copy landed"); ok {
			landed = append(landed, copy)
		}
	}
	return landed
}

// logCopy returns the copy of a JSON log line with message msg.
func logCopy(line, msg string) (string, bool) {
	var entry struct {
		Msg  string `json:"msg"`
		Copy string `json:"copy"`
	}
	if json.Unmarshal([]byte(line), &entry) != nil || entry.Msg != msg {
		return "", false
	}
	return entry.Copy, true
}
