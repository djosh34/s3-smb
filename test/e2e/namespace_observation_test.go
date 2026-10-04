// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceBoundsIncludeDelayedCompletion(t *testing.T) {
	// The final attempt wrote the receipt after one second, but retries or
	// receipt sync delayed startup completion until 61 seconds had elapsed.
	m := namespaceMeasurement{BackupReceiptWriteSeconds: 1, BackupStartupSeconds: 61}
	if err := m.checkBounds(); err == nil || !strings.Contains(err.Error(), "exceeds 60s") {
		t.Fatalf("elapsed startup must fail despite a quick receipt write: %v", err)
	}
	if err := (namespaceMeasurement{BackupStartupSeconds: 60, RecoverySeconds: 60}).checkBounds(); err != nil {
		t.Fatalf("exact time ceiling should pass: %v", err)
	}
}

func TestStagingObservationMayBeUnavailable(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "state", "backup-staging", "snapshot-finished")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "metadata.db"), []byte("staged database"), 0600); err != nil {
		t.Fatal(err)
	}
	// The sampler was not scheduled until staging had already been removed.
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	peak, err := sampleStaging(root, done)
	if err != nil || peak != nil {
		t.Fatalf("missed observation should be unavailable, not a daemon failure: %v %v", peak, err)
	}
	data, err := json.Marshal(namespaceMeasurement{BackupStagingBytes: peak})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"backup_staging_bytes":null`) {
		t.Fatalf("unavailable observation must be recorded as null: %s", data)
	}
}

func TestStagingObservationReportsFilesystemErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	if _, err := sampleStaging(root, done); err == nil {
		t.Fatal("filesystem error was treated as a missed observation")
	}
}
