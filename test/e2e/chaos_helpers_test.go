// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import "testing"

func TestChaosClientAddress(t *testing.T) {
	f := &fixture{addr: "daemon"}
	if got := f.connectAddr(); got != "daemon" {
		t.Fatal(got)
	}
	f.clientAddr = "proxy"
	if got := f.connectAddr(); got != "proxy" || f.addr != "daemon" {
		t.Fatalf("dial address %q, daemon address %q", got, f.addr)
	}
}
