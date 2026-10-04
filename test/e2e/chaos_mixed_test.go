// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	smb "github.com/hirochachacha/go-smb2"
)

type mixedChaosEvent struct {
	name      string
	schedule  chaos.Schedule
	burst     chaosS3Burst
	direction netfault.Direction
	killDelay time.Duration
}

func mixedChaosPlan(seed uint64, gate bool) []mixedChaosEvent {
	random := chaos.Rand(seed, "mixed-schedule")
	rounds, outage := 1, 2*time.Second
	if gate {
		rounds, outage = 3, 5*time.Minute
	}
	var plan []mixedChaosEvent
	for round := range rounds {
		var events []mixedChaosEvent
		for _, phase := range slowNetworkPhases(random.Uint64(), gate) {
			burst := chaosS3Burst{kind: "header-delay", fault: s3fault.Fault{
				Method: http.MethodPut, PathContains: "/chunks/",
				HeaderDelay: time.Duration(10+random.IntN(20)) * time.Millisecond,
			}}
			phase.schedule[0].S3 = &burst.fault
			// Keep the S3 delay through the resumed I/O after a network stall.
			phase.schedule[len(phase.schedule)-1].S3 = &burst.fault
			events = append(events, mixedChaosEvent{name: phase.name, schedule: phase.schedule, burst: burst})
		}
		for _, burst := range chaosS3Bursts(random.Uint64(), gate) {
			// Cold GETs and HTTP timeout exhaustion belong to the focused S3
			// scenario. Mixed work keeps updating and flushing existing bands.
			if burst.fault.Method != http.MethodPut {
				continue
			}
			events = append(events, mixedChaosEvent{name: burst.kind, burst: burst, schedule: chaos.Schedule{
				{Net: &netfault.Fault{Delay: time.Duration(1+random.IntN(4)) * time.Millisecond}, S3: &burst.fault},
				{At: burst.duration, Net: &netfault.Fault{}, S3: &s3fault.Fault{}},
			}})
		}
		for _, direction := range []netfault.Direction{netfault.ClientToServer, netfault.ServerToClient} {
			events = append(events, mixedChaosEvent{name: fmt.Sprintf("cut-%d", direction), direction: direction, schedule: chaos.Schedule{{
				Net: &netfault.Fault{CutAfter: int64(4096 + random.IntN(24<<10)), CutDirection: direction},
			}}})
		}
		events = append(events, mixedChaosEvent{name: "outage", schedule: chaos.Schedule{
			{Net: &netfault.Fault{Delay: time.Duration(1+random.IntN(4)) * time.Millisecond}, S3Outage: outage},
			{At: outage, Net: &netfault.Fault{}, S3: &s3fault.Fault{}},
		}})
		killDelay := time.Duration(20+random.IntN(80)) * time.Millisecond
		if gate {
			killDelay *= 50
		}
		events = append(events, mixedChaosEvent{name: "kill", killDelay: killDelay, schedule: chaos.Schedule{{
			Net: &netfault.Fault{Delay: time.Duration(1+random.IntN(4)) * time.Millisecond},
			S3:  &s3fault.Fault{Method: http.MethodPut, PathContains: "/chunks/", HeaderDelay: time.Duration(10+random.IntN(20)) * time.Millisecond},
		}}})
		random.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
		for _, event := range events {
			event.name = fmt.Sprintf("round-%d/%s", round, event.name)
			plan = append(plan, event)
		}
	}
	return plan
}

func TestChaosMixedFaults(t *testing.T) {
	seed := chaos.Seed(t)
	gate := os.Getenv("S3_SMB_CHECK_MODE") == "gate"
	plan := mixedChaosPlan(seed, gate)
	for i, event := range plan {
		t.Logf("event %d %s kill_delay=%s schedule:\n%s", i, event.name, event.killDelay, event.schedule.String())
	}
	budget := 10 * time.Minute
	if gate {
		budget = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	f := newChaosFixture(t, false)
	f.interval = "1h"
	f.cacheSize = "0 MB"
	proxy := newFaultProxy(t, f.endpoint)
	f.endpoint = proxy.URL()
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
	ledger := chaos.NewLedger()
	data := chaos.Rand(seed, "mixed-initial-data")
	names := []string{"mixed-band-000", "mixed-band-文件", "mixed-earlier-backup"}
	d := chaosKillStart(t, f)
	chaosKillCheckpoint(t, f, ledger, names, data)
	for _, event := range plan {
		data := chaos.Rand(seed, "mixed-data/"+event.name)
		t.Logf("begin %s", event.name)
		if event.killDelay > 0 {
			if err := event.schedule.Run(ctx, network, proxy); err != nil {
				t.Fatal(err)
			}
			chaosKillPendingFlush(t, f, d, proxy, ledger, names[:2], data, event.killDelay)
			d = chaosKillStart(t, f)
			chaosKillCheckFlushed(t, f, ledger)
			// After rule 2, overwrite the entire changed bands and flush. Later
			// rule 1 checks must not require unflushed pre-crash bytes to survive.
			chaosKillCheckpoint(t, f, ledger, names[:2], data)
		} else {
			share, cleanup := f.share()
			closeShare := sync.OnceFunc(cleanup)
			t.Cleanup(closeShare)
			share = share.WithContext(ctx)
			if event.direction != 0 {
				mixedChaosCut(t, ctx, network, proxy, share, ledger, names[0], data, event)
				closeShare()
				share, cleanup = f.share()
				closeShare = sync.OnceFunc(cleanup)
				t.Cleanup(closeShare)
				share = share.WithContext(ctx)
			} else {
				mixedChaosWork(t, ctx, network, proxy, share, ledger, names[:2], data, event)
			}
			if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
				closeShare()
				t.Fatalf("rule 1 after %s: %v", event.name, err)
			}
			closeShare()
		}
		if err := (chaos.Schedule{{Net: &netfault.Fault{}, S3: &s3fault.Fault{}}}).Run(ctx, network, proxy); err != nil {
			t.Fatal(err)
		}
	}
	d.stop()
}

func mixedChaosWork(t *testing.T, ctx context.Context, network *netfault.Proxy, proxy *s3fault.Proxy,
	share *smb.Share, ledger *chaos.Ledger, names []string, data *rand.Rand, event mixedChaosEvent,
) {
	t.Helper()
	for len(proxy.Events()) > 0 {
		<-proxy.Events()
	}
	dropped := proxy.DroppedEvents()
	started := time.Now()
	if err := event.schedule[:1].Run(ctx, network, proxy); err != nil {
		t.Fatal(err)
	}
	scheduleCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- event.schedule[1:].Run(scheduleCtx, network, proxy) }()
	finished := false
	defer func() {
		cancel()
		if !finished {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("%s reset schedule: %v", event.name, err)
			}
		}
		if err := network.SetFault(netfault.Fault{}); err != nil {
			t.Error(err)
		}
		if err := proxy.SetFault(s3fault.Fault{}); err != nil {
			t.Error(err)
		}
	}()
	for round := 0; ; round++ {
		for _, name := range names {
			mixedChaosBand(t, share, ledger, name, data)
		}
		if round == 0 && event.schedule[0].Net.Stall && time.Since(started) < event.schedule[len(event.schedule)-1].At {
			t.Fatalf("%s work finished before the stalled link resumed", event.name)
		}
		select {
		case err := <-done:
			finished = true
			if err != nil {
				t.Fatal(err)
			}
			if proxy.DroppedEvents() != dropped {
				t.Fatalf("%s lost S3 fault events", event.name)
			}
			if event.burst.kind != "" {
				chaosS3ObserveBurst(t, proxy, event.burst)
			} else {
				seen := false
				for len(proxy.Events()) > 0 {
					seen = (<-proxy.Events()).Kind == "outage" || seen
				}
				if !seen {
					t.Fatalf("%s never reached S3 during the outage", event.name)
				}
			}
			t.Logf("%s completed %d band rounds", event.name, round+1)
			return
		default:
		}
	}
}

func mixedChaosBand(t *testing.T, share *smb.Share, ledger *chaos.Ledger, name string, data *rand.Rand) {
	t.Helper()
	file, err := share.OpenFile(name, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	for offset := int64(0); offset < 128<<10; offset += 16 << 10 {
		chaosKillWrite(t, ledger, file, name, offset, chaosKillBytes(data, 16<<10))
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("flush %s: %v", name, err)
	}
	ledger.Flush(name)
}

func mixedChaosCut(t *testing.T, ctx context.Context, network *netfault.Proxy, proxy *s3fault.Proxy,
	share *smb.Share, ledger *chaos.Ledger, name string, data *rand.Rand, event mixedChaosEvent,
) {
	t.Helper()
	operationCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	file, err := share.WithContext(operationCtx).OpenFile(name, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Logf("close cut handle: %v", err)
		}
	}()
	// A successful, unflushed rewrite must survive the following drop.
	chaosKillWrite(t, ledger, file, name, 0, chaosKillBytes(data, 64<<10))
	for len(network.Events()) > 0 {
		<-network.Events()
	}
	dropped := network.DroppedEvents()
	payload := chaosKillBytes(data, 64<<10)
	if event.direction == netfault.ClientToServer {
		ledger.Attempt(name, 64<<10, payload)
	}
	if err := event.schedule.Run(ctx, network, proxy); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var err error
		if event.direction == netfault.ClientToServer {
			var n int
			n, err = file.WriteAt(payload, 64<<10)
			if n > 0 {
				ledger.Write(name, 64<<10, payload[:n])
			}
		} else {
			_, err = file.ReadAt(payload, 0)
		}
		done <- err
	}()
	var forwarded int64
	observed := false
	for !observed {
		select {
		case traffic, ok := <-network.Events():
			if !ok {
				t.Fatal("network proxy closed before cut")
			}
			if traffic.Direction == event.direction {
				forwarded += traffic.Bytes
				observed = traffic.Cut
			}
		case <-operationCtx.Done():
			t.Fatalf("%s cut was not observed: %v", event.name, operationCtx.Err())
		}
	}
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s must fail on the transport, not a client deadline: %v", event.name, err)
		}
		t.Logf("%s visible operation error: %v", event.name, err)
	case <-operationCtx.Done():
		t.Fatalf("%s operation did not finish: %v", event.name, operationCtx.Err())
	}
	if network.DroppedEvents() != dropped || forwarded != event.schedule[0].Net.CutAfter {
		t.Fatalf("%s cut proof: forwarded=%d want=%d lost_events=%d", event.name, forwarded, event.schedule[0].Net.CutAfter, network.DroppedEvents()-dropped)
	}
	if err := network.SetFault(netfault.Fault{}); err != nil {
		t.Fatal(err)
	}
}

func TestMixedChaosPlan(t *testing.T) {
	for _, gate := range []bool{false, true} {
		plan := mixedChaosPlan(358, gate)
		if !reflect.DeepEqual(plan, mixedChaosPlan(358, gate)) {
			t.Fatal("same seed changed the mixed schedule")
		}
		if reflect.DeepEqual(plan, mixedChaosPlan(359, gate)) {
			t.Fatal("different seeds did not change the mixed schedule")
		}
		rounds := 1
		if gate {
			rounds = 3
		}
		if len(plan) != 12*rounds {
			t.Fatalf("gate=%t event count=%d want=%d", gate, len(plan), 12*rounds)
		}
		kinds := make(map[string]int)
		for _, event := range plan {
			_, kind, ok := strings.Cut(event.name, "/")
			if !ok {
				t.Fatalf("event has no round: %s", event.name)
			}
			kinds[kind]++
			if len(event.schedule) == 0 || event.schedule[0].At != 0 {
				t.Fatalf("%s has no initial fault", event.name)
			}
			if kind == "kill" {
				if event.killDelay <= 0 || event.schedule[0].Net.Delay <= 0 || event.schedule[0].S3.HeaderDelay <= 0 {
					t.Fatalf("kill has no mixed faults: %+v", event)
				}
			} else if event.direction != 0 {
				fault := event.schedule[0].Net
				if fault.CutDirection != event.direction || fault.CutAfter < 4096 || fault.CutAfter >= 64<<10 {
					t.Fatalf("invalid cut: %+v", event)
				}
			} else if event.schedule[0].S3Outage > 0 {
				want := 2 * time.Second
				if gate {
					want = 5 * time.Minute
				}
				if event.schedule[0].S3Outage != want || event.schedule[len(event.schedule)-1].At != want {
					t.Fatalf("gate=%t outage length: %s", gate, event.schedule)
				}
			} else if event.burst.kind == "" {
				t.Fatalf("%s has no observable S3 fault", event.name)
			}
			if len(event.schedule) > 1 {
				last := event.schedule[len(event.schedule)-1]
				if last.Net == nil || *last.Net != (netfault.Fault{}) || last.S3Outage != 0 {
					t.Fatalf("%s does not restore the network: %s", event.name, event.schedule)
				}
			}
			var previous time.Duration
			for _, step := range event.schedule {
				if step.At < previous {
					t.Fatalf("unordered event %s: %s", event.name, event.schedule)
				}
				previous = step.At
			}
		}
		for _, kind := range []string{"latency", "jitter", "bandwidth", "stall", "kill", "cut-1", "cut-2", "outage", "status", "throttle", "request-cut", "header-delay"} {
			if kinds[kind] != rounds {
				t.Fatalf("gate=%t %s count=%d want=%d", gate, kind, kinds[kind], rounds)
			}
		}
	}
}
