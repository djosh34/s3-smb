// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestChaosS3Faults runs a backup, a restart and cold reads through S3
// errors, throttling and slow or cut responses. Every SMB request must succeed
// with the right bytes, only slower.
func TestChaosS3Faults(t *testing.T) {
	rng := chaosRand(t)
	count := 6
	if gate() {
		count = 24
	}
	f := chaosFixture(t)
	proxy := f.newFaultProxy()
	d := f.start()
	faults := startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy))
	share, disconnect := f.chaosShare(f.addr)
	files := chaosFiles(rng, "s3-faults", count, 8<<20)
	writeFiles(t, share, files)
	verifyFiles(t, share, files)
	disconnect()
	d.stop()
	d = f.start()
	share, disconnect = f.chaosShare(f.addr)
	verifyFiles(t, share, files)
	disconnect()
	faults.stop()
	d.alive()
}

// TestChaosS3Outage writes a backup while S3 is down for 5 minutes, or 10
// seconds outside the gate. WRITE and FLUSH wait for S3, then the backup
// finishes with the right bytes.
func TestChaosS3Outage(t *testing.T) {
	rng := chaosRand(t)
	outage := 10 * time.Second
	if gate() {
		outage = 5 * time.Minute
	}
	f := chaosFixture(t)
	proxy := f.newFaultProxy()
	d := f.start()
	share, disconnect := f.chaosShare(f.addr)
	defer disconnect()
	files := make(map[string][]byte)
	t.Cleanup(proxy.RestoreS3)
	start := time.Now()
	// The outage begins at a random point of the backup.
	outageStart := time.AfterFunc(between(rng, 0, 2*time.Second), func() { proxy.FailS3For(outage) })
	defer outageStart.Stop()
	for i := 0; time.Since(start) < outage+5*time.Second; i++ {
		name := fmt.Sprintf("outage-%d.bin", i)
		files[name] = chaosData(rng, 1<<20+rng.IntN(16<<20))
		writeFile(t, share, name, files[name])
	}
	select {
	case <-proxy.OutageSeen():
	default:
		t.Fatal("the backup sent no chunk to S3 during the outage")
	}
	verifyFiles(t, share, files)
	d.alive()
}
