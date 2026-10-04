// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestM4Workflow(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/macos.yml")
	must(t, err)
	var workflow struct {
		On struct {
			Dispatch struct {
				Inputs map[string]struct{ Options []string } `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If    string
			Env   map[string]string
			Steps []struct {
				Uses string
				With map[string]string
				Run  string
			}
		}
	}
	must(t, yaml.Unmarshal(data, &workflow))
	if !slices.Contains(workflow.On.Dispatch.Inputs["mode"].Options, "m4") {
		t.Fatal("M4 dispatch mode missing")
	}
	job := workflow.Jobs["m4"]
	if job.If != "inputs.mode == 'm4'" || job.Env["MAC_PHASE"] != "m4" || job.Env["MAC_SERVER"] != "smbnext" {
		t.Fatal("M4 must select its phase and the new server", job)
	}
	checkedOut, ran := false, false
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkedOut = true
			if step.With["ref"] != "" {
				t.Fatal("checkout must use the chosen dispatch ref", step.With)
			}
		}
		if step.Run == "bash test/macos/run.sh" {
			ran = checkedOut
		}
	}
	if !ran {
		t.Fatal("M4 must run the checked-out harness")
	}
	if strings.Contains(workflow.Jobs["machine-loss-recover"].If, "inputs.mode != 'discover'") {
		t.Fatal("M4 must not start machine-loss recovery")
	}
}

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
	for name, count := range map[string]int{"Upload discovery evidence": 1, "Upload backup evidence": 1, "Upload recovery evidence": 1, "Upload M4 evidence": 1, "Upload evidence": 2} {
		if strings.Count(workflow, "- name: "+name+"\n        if: always()") != count {
			t.Fatal("evidence must upload after failure or cancellation", name)
		}
	}
}
