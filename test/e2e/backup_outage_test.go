// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestScheduledMetadataBackupS3Outage cuts S3 just before a scheduled metadata
// backup. SMB metadata changes and buffered writes keep working during the
// outage, and the next metadata backup succeeds once S3 is back.
func TestScheduledMetadataBackupS3Outage(t *testing.T) {
	outage, interval := 8*time.Second, 20*time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage, interval = 300*time.Second, 330*time.Second
	}
	f := newFixture(t, false)
	f.interval = interval.String()
	f.cacheSize = "8 MB"
	proxy := f.newFaultProxy()
	d := f.start()
	share, disconnect := f.share()
	t.Cleanup(disconnect)
	ctx, cancel := context.WithTimeout(context.Background(), interval+outage+2*time.Minute)
	t.Cleanup(cancel)
	share = share.WithContext(ctx)
	file, err := share.Create("backup-outage.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	t.Cleanup(proxy.RestoreS3)
	// The interval covers the outage and the capped retry delay.
	baseline := f.receipt()
	d.running(time.Until(baseline.Snapshot.Add(interval - time.Second)))
	start := proxy.FailS3For(outage)
	select {
	case event := <-proxy.MetadataFailureSeen():
		if event.Method != http.MethodHead {
			t.Fatalf("first failed metadata request is not the backup's HEAD: %+v", event)
		}
	case err := <-d.done:
		d.exited()
		t.Fatalf("daemon exited before the scheduled backup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("scheduled metadata backup did not reach S3")
	}
	var data []byte
	for i := 0; time.Now().Before(start.Add(outage)); i++ {
		name := fmt.Sprintf("during-outage-%d", i)
		if err := share.Mkdir(name, 0o700); err != nil {
			t.Fatal("mkdir during backup retries:", err)
		}
		if err := share.Remove(name); err != nil {
			t.Fatal("remove during backup retries:", err)
		}
		part := []byte("write accepted during metadata backup retries\n")
		if n, err := file.Write(part); err != nil || n != len(part) {
			t.Fatalf("write during backup retries: %d/%d %v", n, len(part), err)
		}
		data = append(data, part...)
		d.running(time.Second)
	}
	if err := file.Sync(); err != nil {
		t.Fatal("flush after S3 returned:", err)
	}
	verifyFiles(t, share, map[string][]byte{"backup-outage.txt": data})
	deadline := time.Now().Add(time.Minute)
	for {
		receipt := f.receipt()
		if receipt.Key != baseline.Key && receipt.Snapshot.After(start) {
			digest := protectionObjectDigest(t, f, protectionObjectKey(t, f, receipt.Key))
			if hex.EncodeToString(digest[:]) != receipt.SHA256 {
				t.Fatal("metadata backup in S3 does not match its receipt")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no metadata backup succeeded after S3 returned")
		}
		d.running(100 * time.Millisecond)
	}
	writeFile(t, share, "after-backup-outage.txt", []byte("writes after backup recovery\n"))
}
