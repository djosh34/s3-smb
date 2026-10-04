package e2e

import (
	"context"
	"fmt"
	"strings"
)

type integrationCommand func(context.Context, string, ...string) (string, error)

// parseTestAllowlist accepts only individual names from a listing, never suite
// selectors or patterns that could silently add tests at a later milestone.
func parseTestAllowlist(allowlist, listing string) ([]string, error) {
	available := make(map[string]bool)
	for _, line := range strings.Split(listing, "\n") {
		available[strings.TrimSpace(line)] = true
	}
	seen := make(map[string]bool)
	var names []string
	for i, line := range strings.Split(allowlist, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if strings.ContainsAny(name, "*?[] \t\r") || !available[name] {
			return nil, fmt.Errorf("allowlist line %d is not an exact test name: %q", i+1, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("allowlist line %d repeats %q", i+1, name)
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}
