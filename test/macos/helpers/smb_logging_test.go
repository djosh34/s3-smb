// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSMBLoggingRestoresPreviousLevel(t *testing.T) {
	for _, previous := range []string{"0", "1", "4"} {
		t.Run(previous, func(t *testing.T) {
			current := previous
			var calls []string
			command := func(args ...string) (string, error) {
				calls = append(calls, strings.Join(args, " "))
				if args[0] == "-n" {
					return current + "\n", nil
				}
				current = strings.TrimPrefix(args[1], "net.smb.fs.loglevel=")
				return "", nil
			}
			state, err := EnableSMBLogging(command)
			must(t, err)
			if state.Previous != previous || state.Active != "1" {
				t.Fatal(state)
			}
			must(t, state.Restore(command))
			want := []string{"-n net.smb.fs.loglevel", "-w net.smb.fs.loglevel=1", "-n net.smb.fs.loglevel", "-w net.smb.fs.loglevel=" + previous}
			if current != previous || !slices.Equal(calls, want) {
				t.Fatal(current, calls)
			}
		})
	}
}

func TestSMBLoggingErrors(t *testing.T) {
	failure := errors.New("sysctl failed")
	for _, failAt := range []int{1, 2, 3} {
		calls := 0
		command := func(...string) (string, error) {
			calls++
			if calls == failAt {
				return "", failure
			}
			return "0", nil
		}
		state, err := EnableSMBLogging(command)
		if !errors.Is(err, failure) {
			t.Fatal(failAt, state, err)
		}
		must(t, state.Restore(command))
		wantCalls := failAt + 1
		if failAt == 1 {
			wantCalls = 1
		}
		if calls != wantCalls {
			t.Fatal("lost cleanup ownership", failAt, state, calls)
		}
	}
	for _, invalid := range []string{"", "-1", "unknown", "4294967296"} {
		state, err := EnableSMBLogging(func(...string) (string, error) { return invalid, nil })
		if err == nil || state.Previous != "" {
			t.Fatal(invalid, state, err)
		}
	}
	state, err := EnableSMBLogging(func(...string) (string, error) { return "0", nil })
	if err == nil || state.Previous != "0" {
		t.Fatal("disabled readback accepted", state, err)
	}
	if err := state.Restore(func(...string) (string, error) { return "", failure }); !errors.Is(err, failure) {
		t.Fatal("lost restoration error", err)
	}
	must(t, (SMBLogging{}).Restore(func(...string) (string, error) {
		t.Fatal("changed logging without a saved level")
		return "", nil
	}))
}
