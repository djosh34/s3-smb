// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func awaitSchedule(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("schedule result = %v, want %v", err, want)
		}
	case <-time.After(testTimeout):
		t.Fatal("schedule did not finish")
	}
	if _, open := <-done; open {
		t.Fatal("schedule result did not close")
	}
}

func awaitFault(t *testing.T, proxy *Proxy, want Fault) {
	t.Helper()
	timer := time.NewTimer(testTimeout)
	defer timer.Stop()
	for {
		proxy.mu.Lock()
		got, changed := proxy.fault, proxy.changed
		proxy.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("fault = %+v, want %+v", got, want)
		}
	}
}

func TestScheduledFaults(t *testing.T) {
	for _, fault := range []Fault{{Delay: time.Hour}, {Stall: true}, {Drop: true}} {
		name := testName(fault, false)
		if fault.Drop {
			name = "drop"
		}
		t.Run(name, func(t *testing.T) {
			peer := startEcho(t)
			proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
			conn := dialProxy(t, proxy)
			exchange(t, conn)
			const restoreAfter = 200 * time.Millisecond
			start := time.Now()
			done, err := proxy.Schedule(t.Context(), []Step{{Fault: fault}, {After: restoreAfter}})
			if err != nil {
				t.Fatal(err)
			}
			awaitFault(t, proxy, fault)
			payload := []byte("scheduled fault preserves traffic")
			if fault.Drop {
				requireDisconnected(t, conn)
				requireDisconnected(t, dialProxy(t, proxy))
			} else {
				writeBytes(t, conn, payload)
				requireBlocked(t, conn)
				readBytes(t, conn, payload)
			}
			awaitSchedule(t, done, nil)
			if elapsed := time.Since(start); elapsed < restoreAfter {
				t.Fatalf("schedule ended early: %v", elapsed)
			}
			exchange(t, dialProxy(t, proxy))
		})
	}
}

func TestScheduledCutAndStepCopy(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	exchange(t, conn)
	steps := []Step{{After: 50 * time.Millisecond, Cut: true}}
	start := time.Now()
	done, err := proxy.Schedule(t.Context(), steps)
	if err != nil {
		t.Fatal(err)
	}
	steps[0] = Step{After: time.Hour, Fault: Fault{Drop: true}}
	requireDisconnected(t, conn)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("cut happened early: %v", elapsed)
	}
	awaitSchedule(t, done, nil)
	exchange(t, dialProxy(t, proxy))
}

func TestScheduleOrderAndCancellation(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Equal times run in order. Cancellation must not clear the final stall.
	done, err := proxy.Schedule(ctx, []Step{
		{Fault: Fault{Drop: true}},
		{Fault: Fault{Stall: true}},
		{After: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitFault(t, proxy, Fault{Stall: true})
	if _, scheduleErr := proxy.Schedule(t.Context(), []Step{{}}); scheduleErr == nil {
		t.Fatal("accepted overlapping schedule")
	}
	cancel()
	awaitSchedule(t, done, context.Canceled)
	conn := dialProxy(t, proxy)
	payload := []byte("cancellation keeps the fault")
	writeBytes(t, conn, payload)
	requireBlocked(t, conn)
	// A new schedule can restore forwarding after the canceled one finishes.
	done, err = proxy.Schedule(t.Context(), []Step{{}})
	if err != nil {
		t.Fatal(err)
	}
	awaitSchedule(t, done, nil)
	readBytes(t, conn, payload)
}

func TestScheduleValidationAndClose(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	for _, steps := range [][]Step{
		nil,
		{{After: -1}},
		{{After: time.Second}, {After: time.Millisecond}},
		{{Fault: Fault{Delay: -1}}},
	} {
		if _, err := proxy.Schedule(t.Context(), steps); err == nil {
			t.Fatalf("accepted invalid steps: %+v", steps)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := proxy.Schedule(ctx, []Step{{}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("schedule with canceled context: %v", err)
	}
	done, err := proxy.Schedule(t.Context(), []Step{{After: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	awaitSchedule(t, done, net.ErrClosed)
	if _, err := proxy.Schedule(t.Context(), []Step{{}}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("schedule after Close: %v", err)
	}
}

func TestConcurrentCommandsAndClose(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	exchange(t, dialProxy(t, proxy))
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 20 {
				if err := proxy.SetFault(Fault{Stall: true}); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				if err := proxy.Cut(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				if err := proxy.SetFault(Fault{}); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
			}
		})
	}
	workers.Go(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	workers.Wait()
}
