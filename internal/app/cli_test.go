// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArguments(t *testing.T) {
	for _, args := range [][]string{{"-c", "cfg", "serve"}, {"serve", "-c", "cfg"}, {"serve", "-c=cfg"}} {
		a, err := parseArguments(args)
		if err != nil || a.command != "serve" || a.configPath != "cfg" {
			t.Fatalf("%v: %+v %v", args, a, err)
		}
	}
	for _, args := range [][]string{{}, {"init"}, {"serve", "-c"}, {"serve", "--log-format", "invalid"}, {"serve", "serve"}} {
		if _, err := parseArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestLogOverrideSurvivesParseFailure(t *testing.T) {
	args := []string{"bad", "--log-format=json"}
	if logOverride(args) != "json" {
		t.Fatal("override lost")
	}
	if _, err := parseArguments(args); err == nil {
		t.Fatal("expected parse failure")
	}
}
func TestConfirmation(t *testing.T) {
	for _, input := range []string{"yes\n", " YES \n", "no\n", "y\n", "yes", "", "yes" + strings.Repeat(" ", 2048) + "\n"} {
		var output bytes.Buffer
		err := confirmOn(strings.NewReader(input), &output, "New dataset")
		want := input == "yes\n" || input == " YES \n"
		if (err == nil) != want {
			t.Errorf("confirmation success=%v want=%v", err == nil, want)
		}
		if !strings.Contains(output.String(), "Continue? [yes/no]: ") {
			t.Fatal("missing prompt")
		}
	}
}
func TestStateLockExcludesAndRetainsInode(t *testing.T) {
	dir := t.TempDir()
	a, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := lockState(dir); err == nil {
		b.Close()
		t.Fatal("second lock succeeded")
	}
	before, err := os.Stat(filepath.Join(dir, "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	after, _ := os.Stat(filepath.Join(dir, "state.lock"))
	if !os.SameFile(before, after) {
		t.Fatal("lock inode changed")
	}
}
func TestStateLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "state.lock")); err != nil {
		t.Fatal(err)
	}
	if l, err := lockState(dir); err == nil {
		l.Close()
		t.Fatal("followed symlink")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "untouched" {
		t.Fatal("mutated target")
	}
}
