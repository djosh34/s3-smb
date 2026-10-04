package e2e

import (
	_ "embed"
	"slices"
	"strings"
	"testing"
)

//go:embed smbtorture-m3.allowlist
var m3TortureCandidates string

//go:embed smbtorture-m3.exclusions
var m3TortureExclusions string

//go:embed testdata/smbtorture-m3.list
var m3PinnedListing string

//go:embed smbtorture-m3.md
var m3SelectionNotes string

func TestM3SambaInventory(t *testing.T) {
	inventory, err := parseTestAllowlist(m3PinnedListing, m3PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	groups := make(map[string]int)
	for _, name := range inventory {
		suffix, found := strings.CutPrefix(name, "smb2.")
		group, _, individual := strings.Cut(suffix, ".")
		if !found || !individual {
			t.Fatalf("not an individual SMB2 test: %s", name)
		}
		groups[group]++
	}
	for group, want := range map[string]int{
		"read": 5, "rw": 3, "getinfo": 8, "setinfo": 1, "dir": 8,
		"rename": 11, "compound": 19, "compound_find": 3, "compound_async": 2,
	} {
		if got := groups[group]; got != want {
			t.Fatalf("pinned %s inventory has %d names, want %d", group, got, want)
		}
	}
	if len(inventory) != 60 {
		t.Fatalf("pinned M3 inventory has %d names, want 60", len(inventory))
	}
	candidates, err := parseTestAllowlist(m3TortureCandidates, m3PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 34 {
		t.Fatalf("M3 has %d candidates, want 34", len(candidates))
	}
	covered := make(map[string]bool)
	for _, name := range candidates {
		covered[name] = true
	}
	for _, line := range strings.Split(m3TortureExclusions, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, found := strings.Cut(line, "\t")
		if !found || !strings.Contains(m3SelectionNotes, "| "+reason+" |") {
			t.Fatalf("exclusion has no source-backed reason: %q", line)
		}
		if !slices.Contains(inventory, name) || covered[name] {
			t.Fatalf("exclusion is unknown, repeated or also a candidate: %q", name)
		}
		covered[name] = true
	}
	for _, name := range inventory {
		if !covered[name] {
			t.Fatalf("pinned name has no candidate or exclusion: %s", name)
		}
	}
}

func TestSambaM3Interop(t *testing.T) {
	names, err := parseTestAllowlist(m3TortureCandidates, m3PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			testSambaInterop(t, name, "quit")
		})
	}
}

func TestSambaM3EmptyRootListing(t *testing.T) {
	testSambaInterop(t, "", "ls")
}

func TestM3SambaCandidatesStayDisabled(t *testing.T) {
	selected := strings.Split(tortureAllowlist, "\n")
	inventory, err := parseTestAllowlist(m3PinnedListing, m3PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range inventory {
		if slices.Contains(selected, name) {
			t.Fatalf("unverified M3 name entered the default gate: %s", name)
		}
	}
}
