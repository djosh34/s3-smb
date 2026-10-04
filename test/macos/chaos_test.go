//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type networkChaosResult struct {
	StatusAtStart string                     `json:"status_at_start"`
	Timing        helpers.NetworkChaosTiming `json:"timing"`
	Plan          helpers.NetworkChaosPlan   `json:"plan"`
	Log           helpers.DropLog            `json:"log"`
	Seed          uint64                     `json:"seed"`
}

func (h *harness) networkChaos(outcome result) result {
	seed := chaos.Seed(h.t)
	h.t.Logf("Mac replay: dispatch macos.yml on this ref with mode=scenarios, server=smbnext, scenarios=[\"network-chaos\"], chaos_seed=%d", seed)
	report := networkChaosResult{Seed: seed, Plan: helpers.NetworkChaos(seed)}
	h.save("network-chaos-plan.json", report)
	h.randomFile("later.bin", 4<<30)
	h.must(os.WriteFile(filepath.Join(h.proof, "nested/message.txt"), []byte("network chaos backup\n"), 0o600))
	updated := filepath.Join(h.evidence, "network-chaos-tree.json")
	h.manifest(h.proof, updated)
	before := h.objects("s3-smb/chunks/")
	started := time.Now().UTC()
	h.startBackup("network-chaos")
	report.StatusAtStart = h.waitForBandWrites(before)
	timing, err := helpers.RunNetworkChaos(h.ctx, h.proxy, report.Plan, func() error {
		if h.backup.exited() {
			return errors.New("backup ended before network chaos faults")
		}
		return nil
	})
	report.Timing = timing
	h.save("network-chaos-timing.json", report)
	h.must(err)
	_, err = h.completeBackup("network-chaos")
	h.must(err)
	report.Log = h.dropLog(started, time.Now().UTC())
	h.save("network-chaos-result.json", report)
	h.must(helpers.CheckChaosBackup(report.Log))
	outcome.NetworkChaos = &report
	h.must(h.detach())
	h.mount()
	latest := h.remoteBackup("chaos-completed", "")
	if filepath.Base(latest) == outcome.Baseline {
		h.t.Fatal("network chaos produced no completed backup")
	}
	outcome.Resumed = filepath.Base(latest)
	outcome.ResumedRestore = h.restore(latest, updated, "restore-chaos")
	h.must(h.detach())
	h.mount()
	baseline := h.remoteBackup("chaos-baseline-intact", outcome.Baseline)
	outcome.BaselineRestore = h.restore(baseline, filepath.Join(h.transfer, "reference/tree.json"), "restore-baseline")
	h.must(h.detach())
	h.finish()
	h.checkChaosLogs()
	return outcome
}

// Run after finish so shutdown and PTY logs are included, even on failure.
func (h *harness) checkChaosLogs() {
	paths, err := filepath.Glob(filepath.Join(h.evidence, "application-*.log"))
	if err != nil {
		h.t.Error(err)
		return
	}
	if len(paths) == 0 {
		h.t.Error("network chaos has no daemon log evidence")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path) //nolint:gosec // These are the daemon logs under the run-owned evidence directory.
		if err != nil {
			h.t.Error(err)
			continue
		}
		if err := chaos.CheckDaemonLog(data); err != nil {
			h.t.Errorf("%s: %v", filepath.Base(path), err)
		}
	}
}
