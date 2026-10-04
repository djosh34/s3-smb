// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"errors"
	"path/filepath"
	"strings"
)

// SelectBackup picks the backup named identifier, or latest when identifier is
// empty, from tmutil's list of completed backups. It must be listed exactly once.
func SelectBackup(backups []string, latest, identifier string) (string, error) {
	var matches []string
	for _, path := range backups {
		if (identifier == "" && path == latest) || (identifier != "" && filepath.Base(path) == identifier) {
			matches = append(matches, path)
		}
	}
	if len(matches) != 1 || !filepath.IsAbs(matches[0]) || strings.HasSuffix(filepath.Base(matches[0]), ".inProgress") {
		return "", errors.New("completed remote backup not present exactly once")
	}
	return matches[0], nil
}
