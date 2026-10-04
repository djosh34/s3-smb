// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	With map[string]string `yaml:"with"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
}

func checkTransfer(t *testing.T, step workflowStep) {
	t.Helper()
	artifact := step.With["name"]
	if strings.Contains(artifact, "run_attempt") || !strings.Contains(artifact, "github.run_id") || step.With["overwrite"] != "true" {
		t.Fatal("transfer changes on rerun", step)
	}
}

func TestWorkflowTransfersAndEvidence(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/macos.yml")
	must(t, err)
	var workflow struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	must(t, yaml.Unmarshal(data, &workflow))
	uploads, downloads := make(map[string]int), make(map[string]int)
	evidenceJobs := 0
	for name, job := range workflow.Jobs {
		evidence := false
		for _, step := range job.Steps {
			artifact := step.With["name"]
			if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				if strings.Contains(step.With["path"], "MAC_TRANSFER") {
					checkTransfer(t, step)
					uploads[artifact]++
				} else {
					if step.If != "always()" || !strings.Contains(step.With["path"], "mac-harness.log") {
						t.Fatal("evidence missing on cancellation", name, step)
					}
					evidence = true
				}
			}
			if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
				downloads[artifact]++
			}
		}
		if !evidence {
			t.Fatal("job has no always-uploaded evidence", name)
		}
		evidenceJobs++
	}
	if evidenceJobs != 5 || len(uploads) != 4 || len(downloads) != 4 {
		t.Fatal(evidenceJobs, uploads, downloads)
	}
	for name, count := range uploads {
		if count != 1 || downloads[name] != 1 {
			t.Fatal("producer and consumer disagree", name, uploads, downloads)
		}
	}
}
