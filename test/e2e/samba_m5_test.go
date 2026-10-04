package e2e

import (
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"
)

// M5 selection is explicit. TestSambaInterop keeps the unchanged M2 list.
//
//go:embed smbtorture.m5.allowlist
var m5TortureCandidates string

//go:embed smbtorture.m5.exclusions
var m5TortureExclusions string

//go:embed testdata/smbtorture-m5.list
var m5PinnedListing string

func m5CandidateNames(t *testing.T) []string {
	t.Helper()
	names, err := parseTortureAllowlist(m5TortureCandidates, m5PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestM5CandidatesUsePinnedIndividualNames(t *testing.T) {
	want := []string{
		"smb2.lease.v2_epoch1.v2_epoch1",
		"smb2.lease.v2_breaking3.v2_breaking3",
		"smb2.lease.v2_complex1.v2_complex1",
		"smb2.lease.v2_complex2.v2_complex2",
		"smb2.lease.v2_rename.v2_rename",
		"smb2.lease.v2_bug15148.v2_bug15148",
	}
	if names := m5CandidateNames(t); !slices.Equal(names, want) {
		t.Fatalf("M5 candidates = %v, want %v", names, want)
	}
}

func TestM5InventoryAccountsForEveryPinnedName(t *testing.T) {
	available, err := parseTortureAllowlist(m5PinnedListing, m5PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 56 {
		t.Fatalf("pinned M5 inventory has %d names, want 56", len(available))
	}
	covered := make(map[string]bool)
	for _, name := range m5CandidateNames(t) {
		covered[name] = true
	}
	reasons := []string{"lease-v1", "directory-leases", "rw-break", "oplocks", "timeout-policy", "app-instance", "persistence", "dynamic-shares"}
	for _, line := range strings.Split(m5TortureExclusions, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, found := strings.Cut(line, "\t")
		if !found || !slices.Contains(reasons, reason) {
			t.Fatalf("exclusion has no documented reason: %q", line)
		}
		if !slices.Contains(available, name) || covered[name] {
			t.Fatalf("exclusion is unknown, duplicated or also selected: %q", name)
		}
		covered[name] = true
	}
	for _, name := range available {
		if !covered[name] {
			t.Fatalf("pinned test has no selection or exclusion: %s", name)
		}
	}
}

func TestM5DoesNotChangeDefaultSelectedList(t *testing.T) {
	selected := strings.Split(tortureAllowlist, "\n")
	for _, name := range m5CandidateNames(t) {
		if slices.Contains(selected, name) {
			t.Fatalf("M5 candidate leaked into the default list: %s", name)
		}
	}
}

func TestSambaM5Interop(t *testing.T) {
	for _, name := range m5CandidateNames(t) {
		t.Run(name, func(t *testing.T) {
			testSambaInterop(t, name)
		})
	}
}

func TestM5RunnerExecutesOnlyExactCandidates(t *testing.T) {
	names := m5CandidateNames(t)
	var ran []string
	run := func(_ context.Context, tool string, args ...string) (string, error) {
		last := args[len(args)-1]
		if last == "--version" {
			version := "Version " + sambaVersion + "\n"
			if tool == "smbtorture" {
				version = "smbtorture " + sambaVersion + "\n" + version
			}
			return version, nil
		}
		if last == "--list" {
			return m5PinnedListing, nil
		}
		if tool == "smbclient" {
			return "", nil
		}
		ran = append(ran, last)
		return "success: " + last + "\n", nil
	}
	if err := runSamba(t.Context(), run, "127.0.0.1:1445", "TimeMachine", "/tmp/auth", m5TortureCandidates); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ran, names) {
		t.Fatalf("runner selected %v, want %v", ran, names)
	}
}
