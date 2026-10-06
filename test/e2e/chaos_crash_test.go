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
// S3 and the network misbehave, and starts it again on the same data folder
// under the same faults. Until the kill the backup must succeed, and every
// flushed file must survive each kill. After the run, a database copy holds
// every flushed file, and a new data folder restores all of them.
func TestChaosKillRestart(t *testing.T) {
	rng := chaosRand(t)
	rounds := 3
	if gate() {
		rounds = 8
	}
	f := newFixture(t)
	// Startup may stop on an S3 error, and launchd starts it again.
	f.keepAlive, f.startupTimeout = true, 2*time.Minute
	s3 := f.newFaultProxy()
	d := f.start()
	network := f.networkProxy()
	s3Schedule := startSchedule(t, rng, s3Faults(t, s3), clearS3Faults(t, s3))
	networkSchedule := startSchedule(t, rng, networkFaults(network, 3*time.Second), clearNetworkFaults(network))
	share, disconnect := f.chaosShare(network.Address())
	flushed := chaosFiles(rng, "baseline", 3, 1<<20)
	writeFiles(t, share, flushed)
	disconnect()
	for round := range rounds {
		// The kill ends this connection, so it is never logged off.
		share, _ = f.chaosShare(network.Address())
		files := chaosFiles(rng, fmt.Sprintf("kill-%d", round), 8, 2<<20)
		ended := make(chan killedBackup, 1)
		go func() { ended <- writeUntilFailure(share, files) }()
		var backup killedBackup
		select {
		case backup = <-ended:
			// Only the kill may stop the backup.
			if backup.err != nil {
				t.Fatalf("backup failed before the kill: %v", backup.err)
			}
		case <-time.After(between(rng, time.Second, 8*time.Second)):
		}
		sigkill(t, d)
		if backup.synced == nil {
			backup = <-ended
		}
		t.Logf("round %d: %d of %d files flushed before the kill", round, len(backup.synced), len(files))
		maps.Copy(flushed, backup.synced)
		d = f.start()
		share, disconnect = f.chaosShare(network.Address())
		verifyFiles(t, share, flushed)
		disconnect()
	}
	s3Schedule.stop()
	networkSchedule.stop()
	d.alive()
	f.copyDatabase(d)
	f.expireKilledLocks()
	recoverTwice(t, f, flushed)
}

// killedBackup is what a backup flushed before it ended, and the error that
// ended it.
type killedBackup struct {
	err    error
	synced map[string][]byte
}

// writeUntilFailure writes and flushes files in name order until a request
// fails.
func writeUntilFailure(share *smb.Share, files map[string][]byte) killedBackup {
	backup := killedBackup{synced: make(map[string][]byte)}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		file, err := share.Create(name)
		if err != nil {
			backup.err = err
			return backup
		}
		if _, err = file.Write(files[name]); err == nil {
			err = file.Sync()
		}
		if err == nil {
			backup.synced[name] = files[name]
		}
		if backup.err = errors.Join(err, file.Close()); backup.err != nil {
			return backup
		}
	}
	return backup
}
