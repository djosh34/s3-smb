// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import "strings"

// SMBMountpoints selects only this run's share, including a proxy's chosen port.
func SMBMountpoints(text, address string) []string {
	var paths []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "(smbfs") || !strings.Contains(line, address+"/TimeMachine on ") {
			continue
		}
		_, tail, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		path, _, ok := strings.Cut(tail, " (")
		if ok {
			paths = append(paths, path)
		}
	}
	return paths
}
