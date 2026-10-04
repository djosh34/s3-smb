// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSparsebundleDetach(t *testing.T) {
	type commandResult struct {
		err    error
		output string
		args   []string
	}
	busy := errors.New("image busy")
	forceFailure := errors.New("forced detach failed")
	for _, tc := range []struct {
		forceErr    error
		name, mount string
		retry       bool
	}{
		{name: "normal", mount: "/Volumes/s3-smb-m4"},
		{name: "force succeeds", mount: "/Volumes/s3-smb-m4", retry: true},
		{name: "both fail", mount: "/Volumes/s3-smb-m4", retry: true, forceErr: forceFailure},
		{name: "invalid attachment"},
		{name: "invalid attachment force succeeds", retry: true},
		{name: "invalid attachment both fail", retry: true, forceErr: forceFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const bundle = "/share/interop.sparsebundle"
			script := []commandResult{
				{args: []string{"/usr/bin/hdiutil", "attach", "-nobrowse", "-plist", bundle}, output: attachment(t, tc.mount)},
				{args: []string{"/sbin/mount"}},
				{args: []string{"/usr/bin/hdiutil", "detach", "/dev/disk42"}},
			}
			if tc.retry {
				script[2].err = busy
				script = append(script,
					commandResult{args: []string{"/sbin/mount"}},
					commandResult{args: []string{"/usr/bin/hdiutil", "detach", "-force", "/dev/disk42"}, err: tc.forceErr})
			}
			calls := 0
			command := func(args ...string) (string, error) {
				if calls >= len(script) || !slices.Equal(args, script[calls].args) {
					t.Fatal("unexpected command", calls, args)
				}
				step := script[calls]
				calls++
				return step.output, step.err
			}
			err := sparsebundleRoundTrip(command, bundle)
			if calls != len(script) {
				t.Fatal("missing cleanup commands", calls, script)
			}
			failed := tc.forceErr != nil
			if failed != errors.Is(err, busy) || failed != errors.Is(err, forceFailure) {
				t.Fatal("wrong detach errors", err)
			}
			if tc.mount == "" && (err == nil || !strings.Contains(err.Error(), "has 0 mounted volumes, want one")) {
				t.Fatal("lost attachment error", err)
			}
			if tc.mount != "" && !failed {
				must(t, err)
			}
		})
	}
}
