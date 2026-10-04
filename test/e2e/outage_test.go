// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestS3FaultProxyOutage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	p := newFaultProxy(t, upstream.URL)
	client := &http.Client{Timeout: time.Second}
	request := func(method, path string, want int) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, p.URL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d", method, path, res.StatusCode, want)
		}
	}
	p.FailS3For(time.Minute)
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		request(method, "/bucket/chunks/fixture", http.StatusServiceUnavailable)
		select {
		case event := <-p.OutageSeen():
			if event.Method != method || event.Status != http.StatusServiceUnavailable {
				t.Fatalf("unexpected outage event: %+v", event)
			}
		default:
			t.Fatal("chunk failure was not observed")
		}
	}
	request(http.MethodGet, "/bucket/meta/fixture", http.StatusServiceUnavailable)
	p.RestoreS3()
	request(http.MethodGet, "/bucket/chunks/fixture", http.StatusNoContent)
	start := p.FailS3For(20 * time.Millisecond)
	time.Sleep(time.Until(start.Add(20 * time.Millisecond)))
	request(http.MethodGet, "/bucket/chunks/fixture", http.StatusNoContent)
}

type outageResult struct {
	err      error
	finished time.Time
}

func TestDataPathS3Outage(t *testing.T) {
	outage := 3 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = 300 * time.Second
	}
	for _, operation := range []string{"FLUSH", "READ"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t, false)
			// This test covers data retries, not scheduled metadata backups.
			f.interval = "1h"
			f.cacheSize = "0 MB"
			p := newFaultProxy(t, f.endpoint)
			f.endpoint = p.URL()
			d := f.start()
			s, closeShare := f.share()
			data := bytes.Repeat([]byte("S3 outage byte-correct fixture\n"), 1024)
			if operation == "READ" {
				writeFile(t, s, "outage.bin", data)
				closeShare()
				d.stop()
				// Restart to drop VFS read pages as well as the disabled chunk cache.
				f.start()
				s, closeShare = f.share()
			}
			ctx, cancel := context.WithTimeout(context.Background(), outage+3*time.Minute)
			t.Cleanup(cancel)
			s = s.WithContext(ctx)
			t.Cleanup(closeShare)
			flags := os.O_RDONLY
			method := http.MethodGet
			if operation == "FLUSH" {
				flags = os.O_CREATE | os.O_RDWR
				method = http.MethodPut
			}
			file, err := s.OpenFile("outage.bin", flags, 0600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			if operation == "FLUSH" {
				if n, err := file.Write(data); err != nil || n != len(data) {
					t.Fatalf("write before outage: %d/%d %v", n, len(data), err)
				}
			}
			t.Cleanup(p.RestoreS3)
			start := p.FailS3For(outage)
			done := make(chan outageResult, 1)
			go func() {
				var err error
				if operation == "FLUSH" {
					err = file.Sync()
				} else {
					got := make([]byte, len(data))
					_, err = io.ReadFull(file, got)
					if err == nil && !bytes.Equal(got, data) {
						err = fmt.Errorf("cold read returned different bytes")
					}
				}
				done <- outageResult{err: err, finished: time.Now()}
			}()
			select {
			case event := <-p.OutageSeen():
				if event.Method != method || event.Status != http.StatusServiceUnavailable || time.Since(start) >= time.Second {
					t.Fatalf("%s did not reach failed S3 in the first second: %+v after %s", operation, event, time.Since(start))
				}
			case result := <-done:
				t.Fatalf("%s finished without reaching failed S3: %v", operation, result.err)
			case <-time.After(time.Second):
				t.Fatalf("%s did not reach S3 in the first second", operation)
			}
			select {
			case result := <-done:
				if result.err != nil {
					t.Fatalf("%s failed during or after the S3 outage: %v", operation, result.err)
				}
				if result.finished.Before(start.Add(outage)) {
					t.Fatalf("%s finished before S3 recovered", operation)
				}
			case <-ctx.Done():
				t.Fatalf("%s did not finish after S3 recovered: %v", operation, ctx.Err())
			}
			verifyFiles(t, s, map[string][]byte{"outage.bin": data})
			t.Logf("%s survived %s outage, completed after %s", operation, outage, time.Since(start))
		})
	}
}
