// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"reflect"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
)

type slowNetworkPhase struct {
	name     string
	schedule chaos.Schedule
}

// Delay changes model jitter on a TCP stream, not packet reordering or loss.
// Each phase starts with its fault active and ends with normal forwarding.
func slowNetworkPhases(seed uint64, gate bool) []slowNetworkPhase {
	random := chaos.Rand(seed, "slow-network-schedule")
	interval, stall := 100*time.Millisecond, 750*time.Millisecond
	base := 3 * time.Millisecond
	if gate {
		interval, stall = 500*time.Millisecond, 10*time.Second
		base = 15 * time.Millisecond
	}
	latency := base + time.Duration(random.IntN(5))*time.Millisecond
	phases := []slowNetworkPhase{
		{name: "latency", schedule: chaos.Schedule{
			{Net: &netfault.Fault{Delay: latency}},
			{At: 6 * interval, Net: &netfault.Fault{}},
		}},
		{name: "jitter"},
		{name: "stall", schedule: chaos.Schedule{
			{Net: &netfault.Fault{Stall: true}},
			{At: stall, Net: &netfault.Fault{}},
		}},
	}
	for i := range 6 {
		delay := base + time.Duration(random.IntN(12))*time.Millisecond
		phases[1].schedule = append(phases[1].schedule, chaos.Step{
			At: time.Duration(i) * interval, Net: &netfault.Fault{Delay: delay},
		})
	}
	phases[1].schedule = append(phases[1].schedule, chaos.Step{At: 6 * interval, Net: &netfault.Fault{}})
	return phases
}

func TestSlowNetworkSchedule(t *testing.T) {
	for _, gate := range []bool{false, true} {
		phases := slowNetworkPhases(352, gate)
		if !reflect.DeepEqual(phases, slowNetworkPhases(352, gate)) {
			t.Fatal("same seed changed the schedule")
		}
		if reflect.DeepEqual(phases, slowNetworkPhases(353, gate)) {
			t.Fatal("different seeds gave the same schedule")
		}
		for _, phase := range phases {
			if len(phase.schedule) < 2 || phase.schedule[0].At != 0 {
				t.Fatalf("%s must start with a fault", phase.name)
			}
			var previous time.Duration
			for _, step := range phase.schedule {
				if step.At < previous || step.Cut || step.Net == nil || step.Net.Drop || step.S3 != nil || step.S3Outage != 0 {
					t.Fatalf("%s contains an unordered or out-of-scope fault: %+v", phase.name, step)
				}
				previous = step.At
			}
			last := phase.schedule[len(phase.schedule)-1]
			if *last.Net != (netfault.Fault{}) {
				t.Fatalf("%s does not restore forwarding", phase.name)
			}
		}
		stall := phases[2].schedule[1].At
		if (gate && stall < 10*time.Second) || (!gate && stall >= time.Second) {
			t.Fatalf("wrong stall duration for gate=%t: %s", gate, stall)
		}
	}
	jitter := slowNetworkPhases(352, false)[1].schedule
	var changed bool
	for _, step := range jitter[1 : len(jitter)-1] {
		changed = changed || step.Net.Delay != jitter[0].Net.Delay
	}
	if !changed {
		t.Fatal("jitter never changes the delay")
	}
}
