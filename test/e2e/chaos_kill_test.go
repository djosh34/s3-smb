// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"math/rand/v2"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	smb "github.com/hirochachacha/go-smb2"
)

func TestChaosKillRestart(t *testing.T) {
	seed := chaos.Seed(t)
	data := chaos.Rand(seed, "kill-data")
	faults := chaos.Rand(seed, "kill-faults")
	// Switch to newChaosFixture when the harness fixture helper lands.
	f := newFixture(t, false)
	f.interval = "1h"
	f.cacheSize = "0 MB"
	s3Proxy := newFaultProxy(t, f.endpoint)
	f.endpoint = s3Proxy.URL()
	network, err := netfault.New(t.Context(), f.addr)
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
	names := []string{"kill-first.bin", "kill-文件.bin", "kill-unchanged.bin"}
	d := chaosKillStart(t, f)
	chaosKillCheckpoint(t, f, ledger, names, data)
	cycles := 3
	gate := os.Getenv("S3_SMB_CHECK_MODE") == "gate"
	if gate {
		cycles = 6
	}
	for cycle := 0; cycle < cycles; cycle++ {
		netDelay := time.Duration(1+faults.IntN(3)) * time.Millisecond
		s3Delay := time.Duration(10+faults.IntN(20)) * time.Millisecond
		killDelay := time.Duration(20+faults.IntN(80)) * time.Millisecond
		if gate {
			netDelay *= 5
			s3Delay *= 10
			killDelay *= 50
		}
		schedule := chaos.Schedule{{
			Net: &netfault.Fault{Delay: netDelay},
			S3:  &s3fault.Fault{Method: http.MethodPut, PathContains: "/chunks/", HeaderDelay: s3Delay},
		}}
		t.Logf("cycle %d: kill %s after held PUT; faults %s", cycle+1, killDelay, schedule.String())
		if err := schedule.Run(t.Context(), network, s3Proxy); err != nil {
			t.Fatal(err)
		}
		chaosKillPendingFlush(t, f, d, s3Proxy, ledger, names[:2], data, killDelay)

		// Keep the same root, cache, bucket and proxy endpoints. Both proxies
		// still apply their faults while the new process starts and is checked.
		d = chaosKillStart(t, f)
		chaosKillCheckFlushed(t, f, ledger)
		clear := chaos.Schedule{{Net: &netfault.Fault{}, S3: &s3fault.Fault{}}}
		if err := clear.Run(t.Context(), network, s3Proxy); err != nil {
			t.Fatal(err)
		}
		// A fresh acknowledged checkpoint proves writes work after restart
		// and gives the next crash a known last-flushed value for every byte.
		chaosKillCheckpoint(t, f, ledger, names, data)
	}
	d.stop()
}

func chaosKillStart(t *testing.T, f *fixture) *daemon {
	t.Helper()
	d := f.start()
	t.Cleanup(func() {
		d.stop()
		for _, file := range d.logs {
			log, err := os.ReadFile(file.Name())
			if err != nil {
				t.Error(err)
				continue
			}
			if err := chaos.CheckDaemonLog(log); err != nil {
				t.Errorf("daemon generation log %s: %v", file.Name(), err)
			}
		}
	})
	return d
}

func chaosKillBytes(rng *rand.Rand, size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(rng.UintN(256))
	}
	return data
}

func chaosKillWrite(t *testing.T, ledger *chaos.Ledger, file *smb.File, name string, offset int64, data []byte) {
	t.Helper()
	ledger.Attempt(name, offset, data)
	n, err := file.WriteAt(data, offset)
	if err != nil || n != len(data) {
		t.Fatalf("write %s at %d: %d/%d bytes: %v", name, offset, n, len(data), err)
	}
	ledger.Write(name, offset, data)
}

func chaosKillCheckpoint(t *testing.T, f *fixture, ledger *chaos.Ledger, names []string, rng *rand.Rand) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	for _, name := range names {
		chaosKillFlushWrite(t, share, ledger, name, chaosKillBytes(rng, 128<<10))
	}
	if err := ledger.CheckAcknowledged(share.ReadFile); err != nil {
		t.Fatalf("acknowledged checkpoint: %v", err)
	}
}

func chaosKillFlushWrite(t *testing.T, share *smb.Share, ledger *chaos.Ledger, name string, data []byte) {
	t.Helper()
	file, err := share.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Less than a JuiceFS block keeps later unflushed writes buffered
	// until the explicit FLUSH, where the proxy observes the real PUT.
	chaosKillWrite(t, ledger, file, name, 0, data)
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	ledger.Flush(name)
}

func chaosKillPendingFlush(t *testing.T, f *fixture, d *daemon, proxy *s3fault.Proxy, ledger *chaos.Ledger, names []string, rng *rand.Rand, delay time.Duration) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	var files []*smb.File
	for _, name := range names {
		file, err := share.OpenFile(name, os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		defer func() {
			if err := file.Close(); err != nil {
				if !d.stopped {
					t.Error(err)
				} else {
					t.Logf("close interrupted handle %s: %v", name, err)
				}
			}
		}()
		for _, offset := range []int64{0, 16 << 10, 16 << 10, 96 << 10} {
			chaosKillWrite(t, ledger, file, name, offset, chaosKillBytes(rng, 16<<10))
		}
	}
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Release()
	pending := make(chan error, 1)
	go func() { pending <- files[0].Sync() }()
	event := waitHeldPut(t, held, pending)
	t.Logf("kill with FLUSH pending on %s; accepted chunk PUT %s", names[0], event.Path)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case err := <-pending:
		t.Fatalf("held FLUSH completed before kill: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	case <-timer.C:
	}
	sigkill(t, d)
	waitInterruptedSMB(t, pending)
}

func chaosKillCheckFlushed(t *testing.T, f *fixture, ledger *chaos.Ledger) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	if err := ledger.CheckFlushed(share.ReadFile); err != nil {
		t.Fatalf("rule 2 after restart: %v", err)
	}
}
