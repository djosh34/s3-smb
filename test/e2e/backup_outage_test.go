// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
)

func TestScheduledMetadataBackupS3Outage(t *testing.T) {
	outage := 8 * time.Second
	interval := 20 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = 300 * time.Second
		interval = 330 * time.Second
	}
	f := newFixture(t, false)
	f.interval = interval.String()
	f.cacheSize = "8 MB"
	proxy := newFaultProxy(t, f.endpoint)
	f.endpoint = proxy.URL()
	d := f.start()
	readReceipt := func() backup.Receipt {
		data, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
		if err != nil {
			t.Fatal(err)
		}
		var receipt backup.Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	checkServing := func() {
		select {
		case err := <-d.done:
			d.stopped = true
			d.closeLogs()
			t.Fatalf("daemon stopped while the last backup still protected writes: %v", err)
		default:
		}
	}
	s, disconnect := f.share()
	t.Cleanup(disconnect)
	ctx, cancel := context.WithTimeout(context.Background(), interval+outage+2*time.Minute)
	t.Cleanup(cancel)
	s = s.WithContext(ctx)
	file, err := s.Create("backup-outage.txt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(proxy.RestoreS3)
	// Finish SMB setup before waiting, then cut S3 just before the next backup
	// is due. The interval covers the outage and the capped retry delay.
	baseline := readReceipt()
	due := time.NewTimer(time.Until(baseline.Snapshot.Add(interval - time.Second)))
	defer due.Stop()
	select {
	case <-due.C:
	case err := <-d.done:
		d.stopped = true
		d.closeLogs()
		t.Fatalf("daemon stopped before the scheduled backup: %v", err)
	}
	start := proxy.FailS3For(outage)
	select {
	case event := <-proxy.MetadataFailureSeen():
		if event.Method != http.MethodHead || event.Status != http.StatusServiceUnavailable {
			t.Fatalf("scheduled metadata backup did not reach failed S3: %+v", event)
		}
	case err := <-d.done:
		d.stopped = true
		d.closeLogs()
		t.Fatalf("daemon stopped before a scheduled backup reached failed S3: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("scheduled metadata backup did not reach failed S3")
	}
	var data bytes.Buffer
	for i := 0; time.Now().Before(start.Add(outage)); i++ {
		checkServing()
		// Metadata writes do not need S3. They must not be disabled by a few
		// failed backups. Small buffered file writes must still be accepted too.
		name := fmt.Sprintf("during-outage-%d", i)
		if err := s.Mkdir(name, 0700); err != nil {
			t.Fatal("metadata write during backup retries:", err)
		}
		if err := s.Remove(name); err != nil {
			t.Fatal("metadata delete during backup retries:", err)
		}
		part := []byte("write accepted during metadata backup retries\n")
		if n, err := file.Write(part); err != nil || n != len(part) {
			t.Fatalf("write during backup retries: %d/%d %v", n, len(part), err)
		}
		if _, err := data.Write(part); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	checkServing()
	if data.Len() == 0 {
		t.Fatal("no writes were tested during the S3 outage")
	}
	if err := file.Sync(); err != nil {
		t.Fatal("flush after S3 recovered:", err)
	}
	verifyFiles(t, s, map[string][]byte{"backup-outage.txt": data.Bytes()})
	deadline := time.Now().Add(time.Minute)
	for {
		checkServing()
		receipt := readReceipt()
		if receipt.Key != baseline.Key && receipt.Snapshot.After(start) {
			key := protectionObjectKey(t, f, receipt.Key)
			digest := protectionObjectDigest(t, f, key)
			if hex.EncodeToString(digest[:]) != receipt.SHA256 {
				t.Fatal("recovered metadata backup does not match its verified receipt")
			}
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("next metadata backup did not succeed after S3 recovered")
		}
		time.Sleep(100 * time.Millisecond)
	}
	writeFile(t, s, "after-backup-outage.txt", []byte("writes after backup recovery\n"))
	t.Logf("scheduled metadata backup and writes survived %s S3 outage without a restart", outage)
}
