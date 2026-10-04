// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
)

// NetworkChaosPlan varies the per-buffer delay, then stalls the same connections.
// Neither phase cuts connections or rejects reconnects.
type NetworkChaosPlan struct {
	Delays []netfault.Step `json:"delays"`
	Stall  time.Duration   `json:"stall"`
}

// NetworkChaos builds an eight-second jitter phase and a five-to-ten-second stall.
func NetworkChaos(seed uint64) NetworkChaosPlan {
	random := chaos.Rand(seed, "mac-network-chaos")
	plan := NetworkChaosPlan{Stall: time.Duration(5_000+random.IntN(5_001)) * time.Millisecond}
	for i := range 8 {
		plan.Delays = append(plan.Delays, netfault.Step{
			After: time.Duration(i) * time.Second,
			Fault: netfault.Fault{Delay: time.Duration(2_000+random.IntN(10_001)) * time.Microsecond},
		})
	}
	plan.Delays = append(plan.Delays, netfault.Step{After: 8 * time.Second})
	return plan
}

// NetworkChaosTiming records wall-clock evidence, not just planned offsets.
type NetworkChaosTiming struct {
	Started  time.Time `json:"started"`
	Stalled  time.Time `json:"stalled"`
	Restored time.Time `json:"restored"`
}

// RunNetworkChaos uses the proxy schedule for jitter. The stall is measured
// separately so a delayed runner cannot count a long stall as a short one.
// active must reject a backup that ended before either fault phase began.
func RunNetworkChaos(ctx context.Context, proxy *netfault.Proxy, plan NetworkChaosPlan, active func() error) (timing NetworkChaosTiming, resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, proxy.SetFault(netfault.Fault{}))
		timing.Restored = time.Now()
		if !timing.Stalled.IsZero() {
			resultErr = errors.Join(resultErr, CheckChaosStall(timing.Stalled, timing.Restored))
		}
	}()
	if plan.Stall <= 0 || plan.Stall >= 30*time.Second {
		return timing, errors.New("network chaos stall must be positive and shorter than 30 seconds")
	}
	if err := active(); err != nil {
		return timing, err
	}
	timing.Started = time.Now()
	done, err := proxy.Schedule(ctx, plan.Delays)
	if err != nil {
		return timing, err
	}
	if err := <-done; err != nil {
		return timing, err
	}
	if err := active(); err != nil {
		return timing, err
	}
	if err := ctx.Err(); err != nil {
		return timing, err
	}
	timing.Stalled = time.Now()
	if err := proxy.SetFault(netfault.Fault{Stall: true}); err != nil {
		return timing, err
	}
	timer := time.NewTimer(plan.Stall)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return timing, ctx.Err()
	case <-timer.C:
		return timing, ctx.Err()
	}
}

// CheckChaosStall rejects a stalled runner outside the reconnect promise.
func CheckChaosStall(start, restored time.Time) error {
	elapsed := restored.Sub(start)
	if start.IsZero() || elapsed <= 0 || elapsed >= 30*time.Second {
		return fmt.Errorf("network chaos stall lasted %s, want less than 30 seconds", elapsed)
	}
	return nil
}

// CheckChaosBackup requires one backup start and no client refusal. Stalls do
// not have to trigger a reconnect, so a reconnect message is not required.
func CheckChaosBackup(log DropLog) error {
	if log.Refused || log.BackupStarts != 1 {
		return errors.New("network chaos did not preserve exactly one backup")
	}
	return nil
}
