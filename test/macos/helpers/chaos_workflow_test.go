// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestChaosWorkflowIsOptIn(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/macos.yml")
	must(t, err)
	var workflow struct {
		On map[string]struct {
			Inputs map[string]struct{ Default string }
		}
		Jobs map[string]struct {
			If    string
			Env   map[string]string
			Steps []struct{ Run string }
		}
	}
	must(t, yaml.Unmarshal(data, &workflow))
	if len(workflow.On) != 1 {
		t.Fatal("Mac chaos must remain manual", workflow.On)
	}
	inputs := workflow.On["workflow_dispatch"].Inputs
	var scenarios []string
	must(t, json.Unmarshal([]byte(inputs["scenarios"].Default), &scenarios))
	if slices.Contains(scenarios, "network-chaos") {
		t.Fatal("Mac chaos entered the default matrix")
	}
	if _, ok := inputs["chaos_seed"]; !ok || inputs["chaos_seed"].Default != "" {
		t.Fatal("missing optional replay seed")
	}
	job := workflow.Jobs["scenario"]
	if job.If != "inputs.mode == 'acceptance' || inputs.mode == 'scenarios'" || job.Env["S3_SMB_CHAOS_SEED"] != "${{ inputs.chaos_seed }}" {
		t.Fatal("changed scenario routing or lost replay seed", job)
	}
	if !strings.Contains(job.Env["MAC_SERVER"], "matrix.scenario == 'network-chaos'") {
		t.Fatal("chaos must build the new server")
	}
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "${{ inputs.chaos_seed }}") {
				t.Fatal("seed must enter through the environment, not shell interpolation")
			}
		}
	}
	wrapper, err := os.ReadFile("../run.sh")
	must(t, err)
	if !strings.Contains(string(wrapper), `"S3_SMB_CHAOS_SEED=${S3_SMB_CHAOS_SEED:-}"`) {
		t.Fatal("sudo dropped the replay seed")
	}
}

func TestChaosBackupSelectionLifecycle(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../chaos_test.go", nil, 0)
	must(t, err)
	var function *ast.FuncDecl
	for _, declaration := range file.Decls {
		if candidate, ok := declaration.(*ast.FuncDecl); ok && candidate.Name.Name == "networkChaos" {
			function = candidate
		}
	}
	if function == nil {
		t.Fatal("missing native chaos scenario")
	}
	held, selections, restores, starts := false, 0, 0, 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch harnessCall(node) {
		case "startBackup":
			starts++
		case "remoteBackup":
			if held {
				t.Fatal("selecting a backup while another is held")
			}
			held = true
			selections++
		case "restore":
			if !held {
				t.Fatal("restore without a held completed backup")
			}
			restores++
		case "detach":
			held = false
		case "mount":
			if held {
				t.Fatal("remount before releasing a backup")
			}
		case "resumeBackup", "stopClient":
			t.Fatal("chaos must finish the same backup, not retry or stop it")
		}
		return true
	})
	if held || selections != 2 || restores != 2 || starts != 1 {
		t.Fatal("must complete once and verify new and baseline backups", held, selections, restores, starts)
	}
}
