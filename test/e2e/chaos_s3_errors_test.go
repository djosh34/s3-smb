// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/s3fault"
	smb "github.com/hirochachacha/go-smb2"
)

type chaosS3Burst struct {
	kind     string
	fault    s3fault.Fault
	duration time.Duration
}

func TestChaosS3Errors(t *testing.T) {
	seed := chaos.Seed(t)
	// Switch to newChaosFixture when the harness adds the smbnext binary helper.
	f := newFixture(t, false)
	f.interval = "1h"
	f.cacheSize = "0 MB"
	proxy := newFaultProxy(t, f.endpoint)
	f.endpoint = proxy.URL()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ledger := chaos.NewLedger()
	bursts := chaosS3Bursts(seed, os.Getenv("S3_SMB_CHECK_MODE") == "gate")

	// Seed separate files for cold GETs. Restart before faults to discard VFS
	// pages as well as the disabled chunk cache, keeping the same local state.
	d := chaosS3Start(t, f)
	share, closeShare := f.share()
	share = share.WithContext(ctx)
	cold := make([][]byte, len(bursts))
	for i := range bursts {
		name := fmt.Sprintf("cold-%02d.bin", i)
		cold[i] = chaosS3WriteFile(t, share, ledger, seed, name, 32)
	}
	closeShare()
	d.stop()

	chaosS3Start(t, f)
	share, closeShare = f.share()
	t.Cleanup(closeShare)
	share = share.WithContext(ctx)
	chunks := 32
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		chunks = 64
	}
	for i, burst := range bursts {
		chaosS3RunBurst(t, ctx, share, proxy, ledger, seed, i, burst, cold[i], chunks)
	}
	if err := ledger.CheckAcknowledged(share.ReadFile); err != nil {
		t.Fatalf("acknowledged backup bytes: %v", err)
	}
	if dropped := proxy.DroppedEvents(); dropped != 0 {
		t.Fatalf("lost %d S3 fault events", dropped)
	}
	t.Logf("backup finished with %d byte-correct files", len(ledger.Names()))
}

func chaosS3Bursts(seed uint64, gate bool) []chaosS3Burst {
	rng := chaos.Rand(seed, "s3-errors-schedule")
	burstLength := 3 * time.Second
	delay := 100 * time.Millisecond
	bodyDelay := 20 * time.Millisecond
	if gate {
		burstLength = 12 * time.Second
		delay = 500 * time.Millisecond
		bodyDelay = 100 * time.Millisecond
	}
	bursts := []chaosS3Burst{
		{kind: "status", fault: s3fault.Fault{Method: http.MethodPut, Status: 500, Code: "InternalError"}},
		{kind: "throttle", fault: s3fault.Fault{Method: http.MethodPut, Status: 503, Code: "SlowDown"}},
		{kind: "header-delay", fault: s3fault.Fault{Method: http.MethodPut, HeaderDelay: delay}},
		{kind: "body-delay", fault: s3fault.Fault{Method: http.MethodGet, BodyDelay: bodyDelay}},
		// Both check modes exceed the 30s HTTP header timeout and 60s chunk
		// timeout. A snapshot keeps delaying this request after the burst ends.
		{kind: "timeout", fault: s3fault.Fault{Method: http.MethodGet, HeaderDelay: 65 * time.Second}},
		{kind: "request-cut", fault: s3fault.Fault{Method: http.MethodPut, CutRequest: true, RequestCutAfter: 8192 + rng.Int64N(8192)}},
		{kind: "response-cut", fault: s3fault.Fault{Method: http.MethodGet, CutBody: true, CutAfter: 8192 + rng.Int64N(8192)}},
	}
	for i := range bursts {
		bursts[i].fault.PathContains = "/chunks/"
		bursts[i].duration = burstLength + time.Duration(rng.IntN(1000))*time.Millisecond
	}
	rng.Shuffle(len(bursts), func(i, j int) { bursts[i], bursts[j] = bursts[j], bursts[i] })
	return bursts
}

func chaosS3RunBurst(t *testing.T, ctx context.Context, share *smb.Share, proxy *s3fault.Proxy, ledger *chaos.Ledger, seed uint64, index int, burst chaosS3Burst, cold []byte, chunks int) {
	t.Helper()
	// Apply offset zero before starting I/O. Run the timed reset on the shared
	// schedule runner so a failed operation cannot leave faults enabled.
	schedule := chaos.Schedule{
		{S3: &burst.fault},
		{At: burst.duration, S3: &s3fault.Fault{}},
	}
	t.Logf("burst %d kind=%s schedule=%s", index, burst.kind, schedule.String())
	if err := schedule[:1].Run(ctx, nil, proxy); err != nil {
		t.Fatal(err)
	}
	scheduleCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var scheduleErr error
	go func() {
		scheduleErr = schedule[1:].Run(scheduleCtx, nil, proxy)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
		if scheduleErr != nil && !errors.Is(scheduleErr, context.Canceled) {
			t.Errorf("S3 schedule: %v", scheduleErr)
		}
		if err := proxy.SetFault(s3fault.Fault{}); err != nil {
			t.Errorf("restore S3: %v", err)
		}
	}()

	started := time.Now()
	readDone := make(chan error, 1)
	go func() {
		name := fmt.Sprintf("cold-%02d.bin", index)
		got, err := share.ReadFile(name)
		if err == nil && !bytes.Equal(got, cold) {
			err = fmt.Errorf("%s: cold read changed acknowledged bytes", name)
		}
		readDone <- err
	}()
	for i := range 2 {
		name := fmt.Sprintf("backup-%02d-%d.bin", index, i)
		chaosS3WriteFile(t, share, ledger, seed, name, chunks)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("%s burst cold read: %v", burst.kind, err)
		}
	case <-ctx.Done():
		t.Fatalf("%s burst did not finish: %v", burst.kind, ctx.Err())
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("%s schedule did not finish: %v", burst.kind, ctx.Err())
	}
	chaosS3ObserveBurst(t, proxy, burst)
	elapsed := time.Since(started)
	if burst.kind == "timeout" && elapsed < 30*time.Second {
		t.Fatalf("request finished in %s without reaching the HTTP header timeout", elapsed)
	}
	t.Logf("burst %d kind=%s completed in %s", index, burst.kind, elapsed)
}

func chaosS3ObserveBurst(t *testing.T, proxy *s3fault.Proxy, burst chaosS3Burst) {
	t.Helper()
	kind := burst.kind
	if kind == "timeout" {
		kind = "header-delay"
	}
	seen := 0
	for {
		select {
		case event := <-proxy.Events():
			if event.Kind != kind || event.Method != burst.fault.Method || event.Status != burst.fault.Status && burst.fault.Status != 0 {
				t.Fatalf("unexpected S3 fault during %s: %+v", burst.kind, event)
			}
			seen++
		default:
			if seen == 0 {
				t.Fatalf("%s burst did not reach a matching S3 request", burst.kind)
			}
			t.Logf("%s reached %d S3 requests", burst.kind, seen)
			return
		}
	}
}

func chaosS3WriteFile(t *testing.T, share *smb.Share, ledger *chaos.Ledger, seed uint64, name string, chunks int) []byte {
	t.Helper()
	file, err := share.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", name, err)
		}
	}()
	ledger.Truncate(name, 0)
	rng := chaos.Rand(seed, "s3-errors-data/"+name)
	data := make([]byte, chunks*32*1024)
	for offset := 0; offset < len(data); offset += 32 * 1024 {
		block := data[offset : offset+32*1024]
		chaosS3Fill(rng, block)
		ledger.Attempt(name, int64(offset), block)
		n, err := file.WriteAt(block, int64(offset))
		if n > 0 {
			ledger.Write(name, int64(offset), block[:n])
		}
		if err != nil {
			t.Fatalf("write %s at %d: %d/%d bytes: %v", name, offset, n, len(block), err)
		}
		if n != len(block) {
			t.Fatalf("write %s at %d: %d/%d bytes: %v", name, offset, n, len(block), io.ErrShortWrite)
		}
		if (offset/len(block)+1)%8 == 0 {
			if err := file.Sync(); err != nil {
				t.Fatalf("flush %s at %d: %v", name, offset, err)
			}
			ledger.Flush(name)
		}
	}
	return data
}

func chaosS3Fill(rng *rand.Rand, data []byte) {
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
}

func chaosS3Start(t *testing.T, f *fixture) *daemon {
	t.Helper()
	d := f.start()
	t.Cleanup(func() {
		d.stop()
		chaosS3CheckLogs(t, d)
	})
	return d
}

func chaosS3CheckLogs(t *testing.T, d *daemon) {
	t.Helper()
	for _, log := range d.logs {
		data, err := os.ReadFile(log.Name())
		if err != nil {
			t.Error(err)
			continue
		}
		if err := chaos.CheckDaemonLog(data); err != nil {
			t.Errorf("%s: %v", log.Name(), err)
		}
	}
}
