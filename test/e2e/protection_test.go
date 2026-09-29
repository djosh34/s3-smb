// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/djosh34/s3-smb/internal/backup"
)

// Actual executable + signed SMB + native S3/MinIO, not a Manager stub. A tiny
// HTTP proxy fails only real metadata backup requests after a verified baseline.
// Chunk data and prior recovery points remain in the real disposable bucket.
func TestScheduledBackupFailure(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, encrypted)
			proxy := newFaultProxy(t, f.endpoint)
			f.endpoint = proxy.URL() // f.store intentionally remains a direct MinIO client.
			d := f.start()
			s, disconnect := f.share()
			fixtures := map[string][]byte{
				"protected-empty":      {},
				"protected-文件.txt":     []byte("verified baseline survives scheduled native metadata failure\n"),
				"protected-blocks.bin": bytes.Repeat([]byte("native-smb-to-minio-baseline-"), 16384),
			}
			for name, data := range fixtures {
				writeFile(t, s, name, data)
			}
			verifyFiles(t, s, fixtures)
			disconnect()
			f.protectedAfter(time.Now())
			receiptBytes, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
			if err != nil {
				t.Fatal(err)
			}
			var receipt backup.Receipt
			if err = json.Unmarshal(receiptBytes, &receipt); err != nil {
				t.Fatal(err)
			}
			key := protectionObjectKey(t, f, receipt.Key)
			baselineDigest := protectionObjectDigest(t, f, key)
			proxy.Record("baseline-receipt-verified")
			failureStart := time.Now()
			proxy.SetMetadataFailure(true)
			select {
			case <-proxy.MetadataFailureSeen():
				proxy.Record("scheduled-metadata-failure-observed")
			case err := <-d.done:
				d.stopped = true
				d.closeLogs()
				t.Fatalf("daemon exited before a scheduled metadata request was faulted: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("no scheduled native metadata PUT/GET reached proxy")
			}
			// App's real total backup budget is two minutes; shutdown is 30 seconds.
			// Fast explicit failures normally exhaust three attempts much sooner. This
			// upper bound must not turn an unbounded/stuck writable process into success.
			deadline := failureStart.Add(2*time.Minute + 35*time.Second)
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case err := <-d.done:
				d.stopped = true
				if err == nil {
					d.closeLogs()
					t.Fatal("scheduled backup failure produced successful process exit")
				}
				d.closeLogs()
				proxy.Record("daemon-nonzero-exit-after-metadata-failure")
				t.Logf("scheduled native backup failure stopped executable in %s: %v", time.Since(failureStart), err)
			case <-timer.C:
				_ = d.cmd.Process.Kill()
				<-d.done
				d.stopped = true
				d.closeLogs()
				t.Fatal("writable executable exceeded backup budget plus bounded shutdown")
			}
			var failureLog bool
			for _, file := range d.logs {
				data, err := os.ReadFile(file.Name())
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte("metadata backup protection failed")) {
					failureLog = true
				}
			}
			if !failureLog {
				t.Fatal("nonzero exit was not attributed to metadata backup protection failure")
			}
			if conn, err := net.DialTimeout("tcp", f.addr, 500*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("SMB listener remained writable after fail-stop")
			}
			if digest := protectionObjectDigest(t, f, key); digest != baselineDigest {
				t.Fatal("failed scheduled operation replaced or corrupted previous recovery point")
			}
			// Remove all daemon-local config/state/cache/key material. Normal recovery
			// must decrypt/load the selected native point and verify EVERY baseline file.
			proxy.SetMetadataFailure(false)
			f.freshLocal()
			restored := f.start()
			recovered, closeRecovered := f.share()
			verifyFiles(t, recovered, fixtures)
			fixtures["resumed-after-failure.txt"] = []byte("writable recovery after fail-stop\n")
			writeFile(t, recovered, "resumed-after-failure.txt", fixtures["resumed-after-failure.txt"])
			verifyFiles(t, recovered, fixtures)
			closeRecovered()
			f.protectedAfter(time.Now())
			restored.stop()
			proxy.Record("all-baseline-hashes-recovered-and-writes-resumed")
		})
	}
}
func protectionObjectKey(t *testing.T, f *fixture, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := f.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range out.Contents {
		candidate := aws.ToString(obj.Key)
		if candidate == key || strings.HasSuffix(candidate, "/"+key) {
			return candidate
		}
	}
	t.Fatalf("durable receipt has no corresponding native MinIO object: %s", key)
	return ""
}
func protectionObjectDigest(t *testing.T, f *fixture, key string) [32]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(out.Body)
	closeErr := out.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return sha256.Sum256(data)
}
