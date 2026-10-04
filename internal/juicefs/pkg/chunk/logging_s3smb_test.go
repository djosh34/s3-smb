// SPDX-License-Identifier: AGPL-3.0-only
package chunk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/logging"
)

type capacityLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *capacityLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *capacityLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCapacityLogBufferConcurrentWrites(t *testing.T) {
	var out capacityLogBuffer
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 100 {
				if _, err := fmt.Fprintln(&out, "line"); err != nil {
					t.Error(err)
				}
			}
		})
	}
	for range 100 {
		_ = out.String()
	}
	workers.Wait()
	if got := out.String(); got != strings.Repeat("line\n", 400) {
		t.Fatalf("concurrent writes lost data: %q", got)
	}
}

func TestNativeCapacityDiagnosticsAreDecimal(t *testing.T) {
	var out capacityLogBuffer
	logging.Install(&out)
	t.Cleanup(func() { logging.Install(nil) })
	if err := logging.Configure("json", "debug"); err != nil {
		t.Fatal(err)
	}
	c := Config{CacheDir: "memory", MaxUpload: 1, MaxDownload: 1, BlockSize: 4 << 20, UploadLimit: 1_000_000, DownloadLimit: 1_000_000, GetTimeout: time.Second}
	c.SelfCheck("logging-test")
	if c.BufferSize != 32<<20 {
		t.Fatalf("formatting patch changed native size: %d", c.BufferSize)
	}
	// Other tests may still log through the global handler.
	done := make(chan struct{})
	go func() {
		defer close(done)
		logger.Info("unrelated MiB to 32 MB")
	}()
	<-done
	want := map[string]string{
		"buffer-size is too small,": fmt.Sprintf("%.6f MB", float64(c.BufferSize)/1e6),
		"max-upload ":               "8.000 Mbps",
		"max-download ":             "8.000 Mbps",
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r struct {
			Level     string `json:"level"`
			Message   string `json:"msg"`
			Component string `json:"component"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Component != "juicefs" {
			continue
		}
		for prefix, units := range want {
			if !strings.HasPrefix(r.Message, prefix) {
				continue
			}
			if !strings.Contains(r.Message, units) || strings.Contains(r.Message, "MiB") || strings.Contains(r.Message, "to 32 MB") {
				t.Fatalf("untruthful decimal status: %s", r.Message)
			}
			if r.Level != "WARN" {
				t.Fatalf("warning lost severity: %v", r)
			}
			delete(want, prefix)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing capacity diagnostics: %v", want)
	}
}
