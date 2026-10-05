// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"
)

// TestChaosKillRestart kills the daemon at a random point of a backup while
// S3 and the network misbehave, and starts it again under the same faults.
// Every flushed file must survive each kill. After the run, cold recovery on
// a new Mac returns every file of the last metadata backup.
func TestChaosKillRestart(t *testing.T) {
	rng := chaosRand(t)
	rounds := 3
	if gate() {
		rounds = 8
	}
	f := chaosFixture(t)
	// Startup refuses to run on some S3 errors, and launchd starts it again.
	f.keepAlive, f.startupTimeout = true, 2*time.Minute
	s3 := f.newFaultProxy()
	d := f.start()
	network := f.networkProxy()
	s3Schedule := startSchedule(t, rng, s3Faults(t, s3), clearS3Faults(t, s3))
	networkSchedule := startSchedule(t, rng, networkFaults(network, 3*time.Second), clearNetworkFaults(network))
	flushed := make(map[string][]byte)
	for round := range rounds {
		// The kill ends this connection, so it is never logged off.
		share, _ := f.chaosShare(network.Address())
		files := chaosFiles(rng, fmt.Sprintf("kill-%d", round), 8, 4<<20)
		synced := make(chan map[string][]byte, 1)
		go func() { synced <- writeUntilKilled(share, files) }()
		time.Sleep(between(rng, 500*time.Millisecond, 5*time.Second))
		sigkill(t, d)
		done := <-synced
		t.Logf("round %d: %d of %d files flushed before the kill", round, len(done), len(files))
		maps.Copy(flushed, done)
		d = f.start()
		share, disconnect := f.chaosShare(network.Address())
		verifyFiles(t, share, flushed)
		disconnect()
	}
	s3Schedule.stop()
	networkSchedule.stop()
	d.alive()
	// Back up soon, so the last metadata backup holds every flushed file.
	d.stop()
	f.interval = ""
	d = f.start()
	f.protectedAfter(time.Now())
	d.stop()
	recoverTwice(t, f, flushed)
}

// writeUntilKilled writes and flushes files in name order until a request
// fails, and returns the files whose flush succeeded.
func writeUntilKilled(share *smb.Share, files map[string][]byte) map[string][]byte {
	synced := make(map[string][]byte)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		file, err := share.Create(name)
		if err != nil {
			return synced
		}
		if _, err = file.Write(files[name]); err == nil {
			err = file.Sync()
		}
		if err == nil {
			synced[name] = files[name]
		}
		if errors.Join(err, file.Close()) != nil {
			return synced
		}
	}
	return synced
}
