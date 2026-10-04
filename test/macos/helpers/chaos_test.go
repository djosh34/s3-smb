// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
)

func TestNetworkChaosReplayAndBounds(t *testing.T) {
	plan := NetworkChaos(359)
	if !reflect.DeepEqual(plan, NetworkChaos(359)) || reflect.DeepEqual(plan, NetworkChaos(360)) {
		t.Fatal("seed did not determine the schedule")
	}
	for seed := range uint64(1_000) {
		plan := NetworkChaos(seed)
		if plan.Stall < 5*time.Second || plan.Stall > 10*time.Second || len(plan.Delays) < 2 {
			t.Fatal("invalid stall or jitter phase", seed, plan)
		}
		var previous time.Duration
		var delays []time.Duration
		for _, step := range plan.Delays[:len(plan.Delays)-1] {
			if step.After < previous || step.Cut || step.Fault.Stall || step.Fault.Drop || step.Fault.CutAfter != 0 || step.Fault.Delay <= 0 || step.Fault.Delay > 12*time.Millisecond {
				t.Fatal("unexpected fault", seed, step)
			}
			previous = step.After
			delays = append(delays, step.Fault.Delay)
		}
		if !slices.ContainsFunc(delays, func(delay time.Duration) bool { return delay != delays[0] }) {
			t.Fatal("jitter did not vary", seed, delays)
		}
		last := plan.Delays[len(plan.Delays)-1]
		if last.After <= previous || last.Cut || last.Fault != (netfault.Fault{}) {
			t.Fatal("delay phase did not restore forwarding", seed, last)
		}
	}
}

func TestChaosStallWindow(t *testing.T) {
	start := time.Now()
	for _, elapsed := range []time.Duration{-time.Second, 0, 30 * time.Second, 31 * time.Second} {
		if err := CheckChaosStall(start, start.Add(elapsed)); err == nil {
			t.Fatal("accepted stall", elapsed)
		}
	}
	must(t, CheckChaosStall(start, start.Add(29*time.Second)))
	if err := CheckChaosStall(time.Time{}, start); err == nil {
		t.Fatal("accepted missing stall timestamp")
	}
}

func TestChaosBackupStartEvidence(t *testing.T) {
	must(t, CheckChaosBackup(DropLog{BackupStarts: 1}))
	must(t, CheckChaosBackup(DropLog{BackupStarts: 1, Reconnected: true}))
	for _, log := range []DropLog{{}, {BackupStarts: 2}, {BackupStarts: 1, Refused: true}} {
		if err := CheckChaosBackup(log); err == nil {
			t.Fatal("accepted missing, restarted or refused backup", log)
		}
	}
}

func TestRunNetworkChaos(t *testing.T) {
	proxy := chaosProxy(t)
	plan := NetworkChaosPlan{Delays: []netfault.Step{{Fault: netfault.Fault{Delay: time.Millisecond}}, {After: 3 * time.Millisecond}}, Stall: 5 * time.Millisecond}
	calls := 0
	timing, err := RunNetworkChaos(t.Context(), proxy, plan, func() error { calls++; return nil })
	must(t, err)
	if calls != 2 || timing.Started.IsZero() || timing.Stalled.Before(timing.Started) || timing.Restored.Sub(timing.Stalled) < plan.Stall {
		t.Fatal("missing active checks or actual stall timing", calls, timing)
	}
	checkChaosForwarding(t, proxy)
}

func TestRunNetworkChaosCanceled(t *testing.T) {
	for _, phase := range []string{"delay", "stall"} {
		t.Run(phase, func(t *testing.T) {
			proxy := chaosProxy(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			plan := NetworkChaosPlan{Delays: []netfault.Step{{Fault: netfault.Fault{Stall: true}}, {After: time.Hour}}, Stall: time.Second}
			if phase == "stall" {
				plan.Delays = []netfault.Step{{}}
			}
			var timer *time.Timer
			calls := 0
			timing, err := RunNetworkChaos(ctx, proxy, plan, func() error {
				calls++
				if (phase == "delay" && calls == 1) || (phase == "stall" && calls == 2) {
					timer = time.AfterFunc(100*time.Millisecond, cancel)
				}
				return nil
			})
			if timer != nil {
				timer.Stop()
			}
			if !errors.Is(err, context.Canceled) || timing.Restored.IsZero() {
				t.Fatal("cancellation or restoration lost", timing, err)
			}
			if phase == "stall" && timing.Stalled.IsZero() {
				t.Fatal("stall never began")
			}
			checkChaosForwarding(t, proxy)
		})
	}
}

func TestRunNetworkChaosRejectsEndedBackup(t *testing.T) {
	for _, endAt := range []int{1, 2} {
		t.Run(strconv.Itoa(endAt), func(t *testing.T) {
			proxy := chaosProxy(t)
			ended := errors.New("backup ended")
			calls := 0
			timing, err := RunNetworkChaos(t.Context(), proxy, NetworkChaosPlan{Delays: []netfault.Step{{}}, Stall: time.Second}, func() error {
				calls++
				if calls == endAt {
					return ended
				}
				return nil
			})
			if !errors.Is(err, ended) || !timing.Stalled.IsZero() {
				t.Fatal("faulted an ended backup", timing, err)
			}
			checkChaosForwarding(t, proxy)
		})
	}
}

func TestRunNetworkChaosInvalidPlan(t *testing.T) {
	for _, plan := range []NetworkChaosPlan{
		{Delays: []netfault.Step{{}}},
		{Delays: []netfault.Step{{}}, Stall: 30 * time.Second},
		{Stall: time.Second},
		{Delays: []netfault.Step{{After: -time.Second}}, Stall: time.Second},
	} {
		proxy := chaosProxy(t)
		must(t, proxy.SetFault(netfault.Fault{Stall: true}))
		timing, err := RunNetworkChaos(t.Context(), proxy, plan, func() error { return nil })
		if err == nil || !timing.Stalled.IsZero() {
			t.Fatal("accepted an invalid plan", timing, err)
		}
		checkChaosForwarding(t, proxy)
	}
}

func chaosProxy(t *testing.T) *netfault.Proxy {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "intact"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	proxy, err := netfault.New(t.Context(), strings.TrimPrefix(server.URL, "http://"))
	must(t, err)
	t.Cleanup(func() { must(t, proxy.Close()) })
	return proxy
}

func checkChaosForwarding(t *testing.T, proxy *netfault.Proxy) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+proxy.Address(), nil)
	must(t, err)
	response, err := client.Do(request)
	must(t, err)
	data, readErr := io.ReadAll(response.Body)
	must(t, errors.Join(readErr, response.Body.Close()))
	if string(data) != "intact" {
		t.Fatal("forwarding corrupted after faults", string(data))
	}
}
