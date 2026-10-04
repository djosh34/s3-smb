// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import "testing"

func TestServerBuildTags(t *testing.T) {
	for _, tc := range []struct {
		server, phase, tags string
		wantError           bool
	}{
		{server: "default", phase: "backup"},
		{server: "smbnext", phase: "backup", tags: "smbnext"},
		{server: "smbnext", phase: "m4", tags: "smbnext"},
		{server: "default", phase: "m4", wantError: true},
		{server: "unknown", phase: "backup", wantError: true},
		{phase: "backup", wantError: true},
	} {
		t.Run(tc.server+"/"+tc.phase, func(t *testing.T) {
			tags, err := ServerBuildTags(tc.server, tc.phase)
			if tags != tc.tags || (err != nil) != tc.wantError {
				t.Fatalf("tags = %q, error = %v; want %q, error %v", tags, err, tc.tags, tc.wantError)
			}
		})
	}
}
