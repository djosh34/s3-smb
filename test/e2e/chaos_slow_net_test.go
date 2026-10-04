// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	smbclient "github.com/hirochachacha/go-smb2"
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
	capDuration, rate := 2*time.Second, int64(512*1024)
	if gate {
		interval, stall = 500*time.Millisecond, 10*time.Second
		base = 15 * time.Millisecond
		capDuration, rate = 10*time.Second, 256*1024
	}
	rate += int64(random.IntN(64 * 1024))
	latency := base + time.Duration(random.IntN(5))*time.Millisecond
	phases := []slowNetworkPhase{
		{name: "latency", schedule: chaos.Schedule{
			{Net: &netfault.Fault{Delay: latency}},
			{At: 6 * interval, Net: &netfault.Fault{}},
		}},
		{name: "jitter"},
		{name: "bandwidth", schedule: chaos.Schedule{
			{Net: &netfault.Fault{BytesPerSecond: rate}},
			{At: capDuration, Net: &netfault.Fault{}},
		}},
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

func TestChaosSlowNetwork(t *testing.T) {
	seed := chaos.Seed(t)
	gate := os.Getenv("S3_SMB_CHECK_MODE") == "gate"
	budget := 2 * time.Minute
	if gate {
		budget = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	f := newChaosFixture(t, true)
	f.interval = "1h"
	s3Proxy, err := s3fault.New(ctx, f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s3Proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	f.endpoint = s3Proxy.URL()
	d := f.start()
	// There is one daemon generation here. Check it after shutdown, including
	// on a failed SMB operation, so race reports at process exit are included.
	t.Cleanup(func() {
		d.stop()
		for _, log := range d.logs {
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Error(err)
				continue
			}
			if err := chaos.CheckDaemonLog(data); err != nil {
				t.Errorf("daemon generation %d, %s: %v", f.generation, log.Name(), err)
			}
		}
	})
	network, err := netfault.New(ctx, f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := network.Close(); err != nil {
			t.Error(err)
		}
	})
	f.clientAddr = network.Address()
	share, closeShare := f.share()
	t.Cleanup(closeShare)
	share = share.WithContext(ctx)
	if err := share.Mkdir("backup.sparsebundle", 0700); err != nil {
		t.Fatal(err)
	}
	ledger := chaos.NewLedger()
	payload := chaos.Rand(seed, "slow-network-payload")
	size := 256 * 1024
	if gate {
		size = 1024 * 1024
	}
	for _, phase := range slowNetworkPhases(seed, gate) {
		t.Logf("%s schedule:\n%s", phase.name, phase.schedule.String())
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(payload.Uint32())
		}
		slowNetworkBackupPhase(t, ctx, network, s3Proxy, share, ledger, phase, data)
	}
	if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
		t.Fatal(err)
	}
}

func slowNetworkBackupPhase(t *testing.T, ctx context.Context, network *netfault.Proxy, s3Proxy *s3fault.Proxy,
	share *smbclient.Share, ledger *chaos.Ledger, phase slowNetworkPhase, data []byte,
) {
	t.Helper()
	var files []*smbclient.File
	var names []string
	defer func() {
		for _, file := range files {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	// Open before applying faults. The stall then interrupts a WRITE rather
	// than a CREATE, and all phases use the same session without reconnecting.
	for i := range 2 {
		name := fmt.Sprintf("backup.sparsebundle/%s-band-%d", phase.name, i)
		file, err := share.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		names = append(names, name)
		ledger.Truncate(name, 0)
	}
	if err := (chaos.Schedule{phase.schedule[0]}).Run(ctx, network, s3Proxy); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	scheduleCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- phase.schedule.Run(scheduleCtx, network, s3Proxy) }()
	finished := false
	defer func() {
		cancel()
		if !finished {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("%s schedule: %v", phase.name, err)
			}
		}
		if err := network.SetFault(netfault.Fault{}); err != nil {
			t.Error(err)
		}
	}()
	// Keep doing band writes and readback through the whole jitter schedule.
	// Rewrites bound the dataset even when the link is fast.
	for round := 0; ; round++ {
		for i, file := range files {
			for offset := 0; offset < len(data); offset += 64 * 1024 {
				slowNetworkWrite(t, ledger, names[i], file, int64(offset), data[offset:offset+64*1024])
				if round == 0 && i == 0 && offset == 0 && phase.name == "stall" && time.Since(started) < phase.schedule[1].At {
					t.Fatal("WRITE finished before the stalled link resumed")
				}
			}
			// Update part of a band, like a backup that revisits existing blocks.
			slowNetworkWrite(t, ledger, names[i], file, 17*1024, data[:8192])
			if err := file.Sync(); err != nil {
				t.Fatalf("flush %s: %v", names[i], err)
			}
			ledger.Flush(names[i])
		}
		if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			finished = true
			if err != nil {
				t.Fatalf("%s schedule: %v", phase.name, err)
			}
			t.Logf("%s completed %d band rounds in %s", phase.name, round+1, time.Since(started))
			return
		default:
		}
	}
}

func slowNetworkWrite(t *testing.T, ledger *chaos.Ledger, name string, file *smbclient.File, offset int64, data []byte) {
	t.Helper()
	ledger.Attempt(name, offset, data)
	n, err := file.WriteAt(data, offset)
	if n > 0 {
		ledger.Write(name, offset, data[:n])
	}
	if err != nil {
		t.Fatalf("write %s at %d: acknowledged %d/%d bytes: %v", name, offset, n, len(data), err)
	}
	if n != len(data) {
		t.Fatalf("write %s at %d: acknowledged %d/%d bytes: %v", name, offset, n, len(data), io.ErrShortWrite)
	}
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
				if step.At < previous || step.Cut || step.Net == nil || step.Net.Drop || step.Net.CutAfter != 0 || step.Net.CutDirection != 0 || step.S3 != nil || step.S3Outage != 0 {
					t.Fatalf("%s contains an unordered or out-of-scope fault: %+v", phase.name, step)
				}
				previous = step.At
			}
			last := phase.schedule[len(phase.schedule)-1]
			if *last.Net != (netfault.Fault{}) {
				t.Fatalf("%s does not restore forwarding", phase.name)
			}
		}
		if phases[2].schedule[0].Net.BytesPerSecond <= 0 {
			t.Fatal("bandwidth phase has no cap")
		}
		stall := phases[3].schedule[1].At
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
