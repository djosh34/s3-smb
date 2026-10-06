// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
)

// The chaos tests run a backup through S3, network and process faults and
// check the promises of "What survives which failure" (#263), "S3 outage a
// backup must survive" (#264) and "What reconnect promises" (#266).

// chaosRand returns the random source of a chaos test, or skips the test when
// S3_SMB_CHAOS_SEED is unset. test/run-linux.sh sets it and prints it; the same
// seed replays the same faults at the same points.
func chaosRand(t *testing.T) *rand.Rand {
	t.Helper()
	value := os.Getenv("S3_SMB_CHAOS_SEED")
	if value == "" {
		t.Skip("needs S3_SMB_CHAOS_SEED: run scripts/check.sh")
	}
	seed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("S3_SMB_CHAOS_SEED: %v", err)
	}
	t.Logf("chaos seed %d: replay with S3_SMB_CHAOS_SEED=%d", seed, seed)
	// Each test has its own stream, so a test replays the same on its own.
	name := fnv.New64a()
	if _, err := name.Write([]byte(t.Name())); err != nil {
		t.Fatal(err)
	}
	return rand.New(rand.NewPCG(seed, name.Sum64())) //nolint:gosec // Test faults need a replayable source.
}

// gate reports whether this is the release gate's full run.
func gate() bool { return os.Getenv("S3_SMB_CHECK_MODE") == "gate" }

// between returns a duration in [low, high).
func between(rng *rand.Rand, low, high time.Duration) time.Duration {
	return low + time.Duration(rng.Int64N(int64(high-low)))
}

// chaosData returns size random bytes.
func chaosData(rng *rand.Rand, size int) []byte {
	data := make([]byte, 0, size+8)
	for len(data) < size {
		data = binary.LittleEndian.AppendUint64(data, rng.Uint64())
	}
	return data[:size]
}

// chaosFiles returns count files of up to maxSize random bytes.
func chaosFiles(rng *rand.Rand, prefix string, count, maxSize int) map[string][]byte {
	files := make(map[string][]byte, count)
	for i := range count {
		files[fmt.Sprintf("%s-%d.bin", prefix, i)] = chaosData(rng, rng.IntN(maxSize))
	}
	return files
}

// writeFiles writes files in name order, so a seed replays the same backup.
func writeFiles(t *testing.T, share *smb.Share, files map[string][]byte) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		writeFile(t, share, name, files[name])
	}
}

// chaosShare connects to addr like the Mac. Its requests may wait out faults
// for up to 10 minutes.
func (f *fixture) chaosShare(addr string) (*smb.Share, func()) {
	f.t.Helper()
	share, disconnect, err := f.connectAt(addr, "backup", f.password)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.t.Context(), 10*time.Minute)
	f.t.Cleanup(cancel)
	return share.WithContext(ctx), disconnect
}

// networkProxy puts a network fault proxy in front of the daemon's SMB port.
func (f *fixture) networkProxy() *netfault.Proxy {
	f.t.Helper()
	proxy, err := netfault.New(f.t.Context(), f.addr)
	if err != nil {
		f.t.Fatal(err)
	}
	closeOnCleanup(f.t, proxy)
	return proxy
}

// faultSchedule applies one fault after another until stopped. Each call of
// next applies a fault drawn from its source and returns how long it lasts.
// The schedule has its own source taken from rng, so the test can keep using
// rng.
type faultSchedule struct {
	done, ended chan struct{}
	clear       func()
	once        sync.Once
}

func startSchedule(t *testing.T, rng *rand.Rand, next func(*rand.Rand) time.Duration, clear func()) *faultSchedule {
	t.Helper()
	source := rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64())) //nolint:gosec // Test faults need a replayable source.
	s := &faultSchedule{done: make(chan struct{}), ended: make(chan struct{}), clear: clear}
	go func() {
		defer close(s.ended)
		for {
			timer := time.NewTimer(next(source))
			select {
			case <-s.done:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	t.Cleanup(s.stop)
	return s
}

// stop ends the schedule and clears the last fault.
func (s *faultSchedule) stop() {
	s.once.Do(func() {
		close(s.done)
		<-s.ended
		s.clear()
	})
}

// s3Faults applies S3 errors, throttling, slow or cut responses, or no fault,
// to GET, PUT or all requests, for up to 2 seconds each.
func s3Faults(t *testing.T, proxy *s3fault.Proxy) func(*rand.Rand) time.Duration {
	methods := []string{"", http.MethodGet, http.MethodPut}
	return func(rng *rand.Rand) time.Duration {
		fault := s3fault.Fault{Method: methods[rng.IntN(len(methods))]}
		switch rng.IntN(5) {
		case 0:
			fault.Status, fault.Code = http.StatusInternalServerError, "InternalError"
		case 1:
			fault.Status, fault.Code = http.StatusServiceUnavailable, "SlowDown"
		case 2:
			fault.HeaderDelay = between(rng, 100*time.Millisecond, 3*time.Second)
		case 3:
			fault.Cut = true
		}
		if err := proxy.SetFault(fault); err != nil {
			t.Error(err)
		}
		return between(rng, 200*time.Millisecond, 2*time.Second)
	}
}

// clearS3Faults is the clear function of an s3Faults schedule.
func clearS3Faults(t *testing.T, proxy *s3fault.Proxy) func() {
	return func() {
		if err := proxy.SetFault(s3fault.Fault{}); err != nil {
			t.Error(err)
		}
	}
}

// networkFaults makes the link slow and unsteady: each step changes its delay
// and rate, and one in eight stalls it for up to maxStall.
func networkFaults(proxy *netfault.Proxy, maxStall time.Duration) func(*rand.Rand) time.Duration {
	return func(rng *rand.Rand) time.Duration {
		if rng.IntN(8) == 0 {
			stall := between(rng, time.Second, maxStall)
			proxy.Stall(stall)
			return stall
		}
		shape := netfault.Shape{Delay: between(rng, 0, 20*time.Millisecond)}
		if rng.IntN(2) == 0 {
			shape.Rate = 256<<10 + rng.IntN(8<<20)
		}
		proxy.SetShape(shape)
		return between(rng, 200*time.Millisecond, 1500*time.Millisecond)
	}
}

// clearNetworkFaults is the clear function of a networkFaults schedule.
func clearNetworkFaults(proxy *netfault.Proxy) func() {
	return func() {
		proxy.Stall(0)
		proxy.SetShape(netfault.Shape{})
	}
}
