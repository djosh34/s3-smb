// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestEjectRefreshesLateSnapshot(t *testing.T) {
	const snapshot = "/Volumes/.timemachine/id/2099.backup"
	busy := errors.New("resource busy")
	mounted := false
	var commands []string
	run := func(args ...string) (string, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "/sbin/mount":
			if mounted {
				return "com.apple.TimeMachine.2099.backup@/dev/disk5s1 on " + snapshot + " (apfs, read-only)\n", nil
			}
			return "", nil
		case "/sbin/umount":
			if !slices.Equal(args, []string{"/sbin/umount", snapshot}) {
				t.Fatal(args)
			}
			mounted = false
			return "", nil
		case "/usr/bin/hdiutil":
			if !slices.Contains(args, "-force") {
				mounted = true
				return "", busy
			}
			if mounted {
				return "", busy
			}
			return "", nil
		default:
			t.Fatal(args)
			return "", errors.New("unknown command")
		}
	}
	if err := Eject(run, "/dev/disk4", false); !errors.Is(err, busy) {
		t.Fatal(err)
	}
	must(t, Eject(run, "/dev/disk4", true))
	expected := []string{"/sbin/mount", "/usr/bin/hdiutil detach /dev/disk4", "/sbin/mount", "/sbin/umount " + snapshot, "/usr/bin/hdiutil detach -force /dev/disk4"}
	if !slices.Equal(commands, expected) {
		t.Fatal("snapshot ordering", commands)
	}
}

func TestUnmountSnapshots(t *testing.T) {
	sentinel := errors.New("native command failed")
	for _, failure := range []string{"mount", "unmount", ""} {
		var commands []string
		run := func(args ...string) (string, error) {
			commands = append(commands, strings.Join(args, " "))
			if args[0] == "/sbin/mount" {
				if failure == "mount" {
					return "", sentinel
				}
				return "com.apple.TimeMachine.local@/dev/disk1 on /Volumes/com.apple.TimeMachine.localsnapshots/source (apfs)\ncom.apple.TimeMachine.remote@/dev/disk5 on /Volumes/.timemachine/id/backup (apfs)\n", nil
			}
			if len(args) == 2 || failure == "unmount" {
				return "", sentinel
			}
			return "", nil
		}
		err := UnmountSnapshots(run)
		if (failure != "") != errors.Is(err, sentinel) {
			t.Fatal(failure, err)
		}
		if failure != "mount" && !slices.Equal(commands, []string{"/sbin/mount", "/sbin/umount /Volumes/.timemachine/id/backup", "/sbin/umount -f /Volumes/.timemachine/id/backup"}) {
			t.Fatal(commands)
		}
	}
}
