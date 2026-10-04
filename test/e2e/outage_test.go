// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"
)

// TestDataPathS3Outage starts a flush or a cold read during an S3 outage. The
// operation must wait for S3 and then succeed with the right bytes.
func TestDataPathS3Outage(t *testing.T) {
	outage := 3 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = 300 * time.Second
	}
	for _, operation := range []string{"FLUSH", "READ"} {
		t.Run(operation, func(t *testing.T) { testDataPathOutage(t, operation, outage) })
	}
}

func testDataPathOutage(t *testing.T, operation string, outage time.Duration) {
	data := bytes.Repeat([]byte("S3 outage byte-correct fixture\n"), 1024)
	f := newFixture(t, false)
	// This test covers data retries, not scheduled metadata backups.
	f.interval = "1h"
	proxy := f.newFaultProxy()
	ctx, cancel := context.WithTimeout(context.Background(), outage+3*time.Minute)
	t.Cleanup(cancel)
	share := outageShare(t, f, operation, data).WithContext(ctx)
	file, err := share.OpenFile("outage.bin", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, file)
	if operation == "FLUSH" {
		// Leave the data unflushed until the outage.
		if n, err := file.Write(data); err != nil || n != len(data) {
			t.Fatalf("write before outage: %d/%d %v", n, len(data), err)
		}
	}
	t.Cleanup(proxy.RestoreS3)
	start := proxy.FailS3For(outage)
	type result struct {
		err      error
		finished time.Time
	}
	done := make(chan result, 1)
	go func() {
		var err error
		if operation == "FLUSH" {
			err = file.Sync()
		} else {
			err = readAll(file, data)
		}
		done <- result{err: err, finished: time.Now()}
	}()
	method := map[string]string{"FLUSH": http.MethodPut, "READ": http.MethodGet}[operation]
	select {
	case event := <-proxy.OutageSeen():
		if event.Method != method {
			t.Fatalf("%s sent %s to S3, want %s", operation, event.Method, method)
		}
	case r := <-done:
		t.Fatalf("%s finished without reaching S3 during the outage: %v", operation, r.err)
	case <-ctx.Done():
		t.Fatalf("%s did not reach S3: %v", operation, ctx.Err())
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("%s failed: %v", operation, r.err)
		}
		if r.finished.Before(start.Add(outage)) {
			t.Fatalf("%s finished before S3 returned", operation)
		}
	case <-ctx.Done():
		t.Fatalf("%s did not finish after S3 returned: %v", operation, ctx.Err())
	}
	verifyFiles(t, share, map[string][]byte{"outage.bin": data})
}

// readAll reads the file and compares it with want.
func readAll(file *smb.File, want []byte) error {
	got := make([]byte, len(want))
	if _, err := io.ReadFull(file, got); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("cold read returned different bytes")
	}
	return nil
}

// outageShare starts the daemon and connects. For READ it first writes
// outage.bin and restarts, so the data is only in S3.
func outageShare(t *testing.T, f *fixture, operation string, data []byte) *smb.Share {
	t.Helper()
	d := f.start()
	share, disconnect := f.share()
	if operation == "READ" {
		writeFile(t, share, "outage.bin", data)
		disconnect()
		d.stop()
		f.start()
		share, disconnect = f.share()
	}
	t.Cleanup(disconnect)
	return share
}
