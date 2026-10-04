//go:build !smbnext

// SPDX-License-Identifier: AGPL-3.0-only
package app

import "testing"

func TestDefaultBuildSelectsOldServer(t *testing.T) {
	r, path := serverResources(t)
	if err := r.startSMB(t.Context(), serverConfig(), path); err != nil {
		t.Fatal(err)
	}
	s, ok := r.server.(*oldServer)
	if !ok {
		t.Fatalf("default server type %T", r.server)
	}
	shares := s.Shares()
	if len(shares) != 1 || shares[0] != "Backups" {
		t.Fatalf("configured shares: %v", shares)
	}
}
