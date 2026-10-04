// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBuildCheckout(t *testing.T) {
	for server, tags := range map[string]string{"default": "", "smbnext": "smbnext"} {
		t.Run(server, func(t *testing.T) {
			var calls [][]string
			metadata, err := Build(t.Context(), "/repo", "/bin", "/source", server, strings.Repeat("b", 40), func(_ context.Context, dir string, args ...string) (string, error) {
				calls = append(calls, append([]string{dir}, args...))
				return "binary metadata", nil
			})
			must(t, err)
			if metadata != "binary metadata" || len(calls) != 11 {
				t.Fatal(metadata, calls)
			}
			if !reflect.DeepEqual(calls[0], []string{"/repo", "go", "build", "-p", "2", "-tags", tags, "-o", "/bin/s3-smb", "."}) {
				t.Fatal(calls[0])
			}
			if !reflect.DeepEqual(calls[3], []string{"/repo", "go", "version", "-m", "/bin/s3-smb"}) {
				t.Fatal(calls[3])
			}
			if !reflect.DeepEqual(calls[6], []string{"/source", "git", "fetch", "--depth", "1", "origin", strings.Repeat("b", 40)}) {
				t.Fatal(calls[6])
			}
			for _, index := range []int{8, 10} {
				if slices.Contains(calls[index], "-tags") {
					t.Fatal("server tag leaked to tooling", calls[index])
				}
			}
			for _, call := range calls {
				if slices.Contains(call, "install") {
					t.Fatal("used public proxy", call)
				}
			}
		})
	}
}

func TestBuildStopsOnError(t *testing.T) {
	failure := errors.New("compiler or metadata failed")
	for _, failAt := range []int{0, 3} {
		calls := 0
		_, err := Build(t.Context(), "/repo", "/bin", "/source", "default", strings.Repeat("b", 40), func(_ context.Context, _ string, _ ...string) (string, error) {
			calls++
			if calls == failAt+1 {
				return "", failure
			}
			return "", nil
		})
		if !errors.Is(err, failure) || calls != failAt+1 {
			t.Fatal(err, calls)
		}
	}
	calls := 0
	_, err := Build(t.Context(), "/repo", "/bin", "/source", "other", "", func(_ context.Context, _ string, _ ...string) (string, error) { calls++; return "", nil })
	if err == nil || calls != 0 {
		t.Fatal("invalid server launched compiler", calls, err)
	}
}
