// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

// TestScheduledBackupFailure fails every metadata backup after a good one. The
// daemon must stop with an error once protection expires, keep the earlier
// backup intact, and recover from it with no local state.
func TestScheduledBackupFailure(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, encrypted)
			proxy := f.newFaultProxy()
			d := f.start()
			share, disconnect := f.share()
			files := map[string][]byte{
				"protected-empty":      {},
				"protected-文件.txt":     []byte("verified baseline survives scheduled native metadata failure\n"),
				"protected-blocks.bin": bytes.Repeat([]byte("native-smb-to-minio-baseline-"), 16384),
			}
			for name, data := range files {
				writeFile(t, share, name, data)
			}
			disconnect()
			f.protectedAfter(time.Now())
			key := protectionObjectKey(t, f, f.receipt().Key)
			baselineDigest := protectionObjectDigest(t, f, key)
			proxy.SetMetadataFailure(true)
			waitFailStop(t, d, proxy)
			if !bytes.Contains(d.output(), []byte("metadata backup protection failed")) {
				t.Fatal("daemon exit was not attributed to failed metadata backups")
			}
			if conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", f.addr); err == nil {
				t.Fatal(errors.Join(errors.New("SMB port still open after the daemon stopped"), conn.Close()))
			}
			if digest := protectionObjectDigest(t, f, key); digest != baselineDigest {
				t.Fatal("failed backups changed the earlier metadata backup")
			}
			proxy.SetMetadataFailure(false)
			f.freshLocal()
			d = f.start()
			share, disconnect = f.share()
			verifyFiles(t, share, files)
			files["resumed-after-failure.txt"] = []byte("writable recovery after fail-stop\n")
			writeFile(t, share, "resumed-after-failure.txt", files["resumed-after-failure.txt"])
			verifyFiles(t, share, files)
			disconnect()
			f.protectedAfter(time.Now())
			d.stop()
		})
	}
}

// waitFailStop waits for a failed metadata backup and then for the daemon to
// exit with an error. Retries stop when protection expires, then shutdown
// takes at most 30 seconds.
func waitFailStop(t *testing.T, d *daemon, proxy *s3fault.Proxy) {
	t.Helper()
	start := time.Now()
	select {
	case <-proxy.MetadataFailureSeen():
	case err := <-d.done:
		d.exited()
		t.Fatalf("daemon exited before a metadata backup failed: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no scheduled metadata backup reached S3")
	}
	timer := time.NewTimer(time.Until(start.Add(2*time.Minute + 35*time.Second)))
	defer timer.Stop()
	select {
	case err := <-d.done:
		d.exited()
		if err == nil {
			t.Fatal("daemon exited successfully after metadata backups failed")
		}
	case <-timer.C:
		t.Fatalf("daemon kept running after metadata backups failed: %v", d.kill())
	}
}

// protectionObjectKey returns the S3 key of a metadata backup.
func protectionObjectKey(t *testing.T, f *fixture, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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

// protectionObjectDigest returns the SHA-256 of an S3 object.
func protectionObjectDigest(t *testing.T, f *fixture, key string) [32]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
