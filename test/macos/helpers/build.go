// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import "errors"

// ServerBuildTags selects the daemon build. M4 must exercise the new server.
func ServerBuildTags(server, phase string) (string, error) {
	if phase == "m4" && server != "smbnext" {
		return "", errors.New("M4 acceptance requires MAC_SERVER=smbnext")
	}
	switch server {
	case "default":
		return "", nil
	case "smbnext":
		return "smbnext", nil
	default:
		return "", errors.New("MAC_SERVER must be default or smbnext")
	}
}
