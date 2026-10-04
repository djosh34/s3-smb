// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"os"
	"strings"
	"testing"
)

func TestWorkflowHandoff(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/macos.yml")
	must(t, err)
	workflow := string(data)
	for _, prefix := range []string{"mac-reference", "mac-store", "mac-loss-reference", "mac-loss-store"} {
		name := "name: " + prefix + "-${{ github.run_id }}"
		if strings.Count(workflow, name+"\n") != 2 || !strings.Contains(workflow, name+"\n          overwrite: true") {
			t.Fatal("transfer names must match across attempts", prefix)
		}
	}
	for name, count := range map[string]int{"Upload discovery evidence": 1, "Upload backup evidence": 1, "Upload recovery evidence": 1, "Upload evidence": 2} {
		if strings.Count(workflow, "- name: "+name+"\n        if: always()") != count {
			t.Fatal("evidence must upload after failure or cancellation", name)
		}
	}
}
