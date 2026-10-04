package e2e

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"strings"
	"testing"
)

//go:embed smbtorture.m4.allowlist
var m4TortureAllowlist string

//go:embed smbtorture.m4.inventory
var m4TortureInventory string

//go:embed testdata/smbtorture-m4.list
var m4PinnedListing string

func TestM4Inventory(t *testing.T) {
	available, err := parseTortureAllowlist(m4PinnedListing, m4PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 63 {
		t.Fatalf("M4 inventory has %d IDs, want 58 family IDs and 5 M3 deferrals", len(available))
	}
	selected, err := parseTortureAllowlist(m4TortureAllowlist, m4PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool)
	for _, line := range strings.Split(m4TortureInventory, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[2] == "" {
			t.Fatalf("inventory needs ID, state and source requirement: %q", line)
		}
		name, state := fields[0], fields[1]
		if !slices.Contains(available, name) || covered[name] {
			t.Fatalf("unknown or repeated inventory ID: %s", name)
		}
		if !slices.Contains([]string{"selected", "probe", "blocked", "excluded"}, state) {
			t.Fatalf("unknown inventory state for %s: %s", name, state)
		}
		if slices.Contains(selected, name) != (state == "selected") {
			t.Fatalf("selection disagrees with inventory for %s: %s", name, state)
		}
		covered[name] = true
	}
	for _, name := range available {
		if !covered[name] {
			t.Fatalf("unaccounted pinned ID: %s", name)
		}
	}
}

// The Linux integration command still selects only TestSambaInterop.
// M4 runs explicitly after the feature owners agree activation.
func TestSambaM4Interop(t *testing.T) {
	testSambaInterop(t, m4TortureAllowlist, "quit")
}

func TestM4Runner(t *testing.T) {
	names, err := parseTortureAllowlist(m4TortureAllowlist, m4PinnedListing)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("staged selection lost the prior passing sharing/create evidence")
	}
	commandErr := errors.New("supported test failed")
	for _, tt := range []struct {
		name string
		fail bool
	}{
		{name: "exact selection"},
		{name: "supported failure blocks", fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ran []string
			run := func(_ context.Context, tool string, args ...string) (string, error) {
				last := args[len(args)-1]
				switch last {
				case "--version":
					version := "Version " + sambaVersion + "\n"
					if tool == "smbtorture" {
						version = "smbtorture " + sambaVersion + "\n" + version
					}
					return version, nil
				case "--list":
					return m4PinnedListing, nil
				case "quit":
					return "", nil
				default:
					ran = append(ran, last)
					if tt.fail {
						return "failure: " + last + "\n", commandErr
					}
					return "success: " + last + "\n", nil
				}
			}
			err := runSamba(t.Context(), run, "127.0.0.1:1445", "TimeMachine", "/tmp/auth", m4TortureAllowlist, "quit")
			want := names
			if tt.fail {
				want = names[:1]
				if !errors.Is(err, commandErr) {
					t.Fatalf("supported failure was lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ran, want) {
				t.Fatalf("runner executed %v, want %v", ran, want)
			}
		})
	}
}
