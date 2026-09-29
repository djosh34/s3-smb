// SPDX-License-Identifier: AGPL-3.0-only
package credentials_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/logging"
	"github.com/djosh34/s3-smb/internal/storage"
)

const access = "s3smb-test-access"
const secret = "s3smb-test-secret-only"
const phrase = "credentials-passphrase-private-marker"
const helperMarker = "credentials-helper-output-private-marker"

// The same test executable supplies direct-argv credentials and records each
// invocation. It is not a shell, fake S3 store, or replacement credential manager.
func TestCredentialHelper(t *testing.T) {
	i := 0
	for ; i < len(os.Args); i++ {
		if os.Args[i] == "credential-acceptance-helper" {
			break
		}
	}
	if i == len(os.Args) {
		return
	}
	args := os.Args[i+1:]
	if len(args) != 3 {
		os.Exit(19)
	}
	f, err := os.OpenFile(args[1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(20)
	}
	if _, err = f.WriteString("invoked\n"); err != nil {
		os.Exit(21)
	}
	if err = f.Close(); err != nil {
		os.Exit(22)
	}
	if args[0] == "fail" {
		fmt.Fprintln(os.Stdout, helperMarker)
		fmt.Fprintln(os.Stderr, helperMarker)
		os.Exit(23)
	}
	fmt.Print(args[2] + "\r\n")
	os.Exit(0)
}
func helper(mode, count, value string) []string {
	return []string{os.Args[0], "-test.run=^TestCredentialHelper$", "--", "credential-acceptance-helper", mode, count, value}
}
func encoded(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		panic("test JSON encoding failed")
	}
	return string(b)
}
func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal("write fixture failed")
	}
}
func source(t *testing.T, dir, name, kind, value string) string {
	t.Helper()
	switch kind {
	case "value":
		return "{value: " + encoded(value) + "}"
	case "file":
		write(t, filepath.Join(dir, name+".secret"), []byte(value+"\r\n"), 0600)
		return "{file: " + encoded("./"+name+".secret") + "}"
	case "command":
		return "{command: " + encoded(helper("value", name+".count", value)) + "}"
	default:
		t.Fatal("invalid fixture source")
		return ""
	}
}
func load(t *testing.T, dir, endpoint, accessSource, secretSource, encryption string) *config.Config {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	text := fmt.Sprintf(`smb:
  username: backup
  password: ""
storage:
  state_dir: ./state
  cache_dir: ./cache
s3:
  bucket: credentials-test
  region: us-east-1
  endpoint: %s
  path_style: true
  access_key: %s
  secret_key: %s
encryption:
%s
`, encoded(endpoint), accessSource, secretSource, encryption)
	write(t, path, []byte(text), 0600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %s", logging.Redact(err.Error()))
	}
	return cfg
}

type logBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *logBuffer) String() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }
func capture(t *testing.T, format string) *logBuffer {
	t.Helper()
	b := new(logBuffer)
	logging.Install(b)
	if err := logging.Configure(format, "debug"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		logging.Install(os.Stderr)
		assertSafe(t, b.String())
		if format == "json" {
			for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
				if line != "" && !json.Valid([]byte(line)) {
					t.Error("diagnostic stream contains non-JSON output")
				}
			}
		}
	})
	return b
}
func assertSafe(t *testing.T, text string) {
	t.Helper()
	for _, value := range []string{access, secret, phrase, helperMarker, "credential-file-path-private-marker", "argument-private-marker"} {
		if strings.Contains(text, value) {
			t.Error("synthetic private marker leaked")
		}
	}
}
func resolve(t *testing.T, cfg *config.Config) *config.Resolved {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := cfg.Resolve(ctx, slog.Default())
	if err != nil {
		assertSafe(t, err.Error())
		t.Fatalf("resolve fixture: %s", logging.Redact(err.Error()))
	}
	return r
}
func roundTrip(t *testing.T, r *config.Resolved, suffix string) {
	t.Helper()
	store, err := storage.OpenS3(r)
	if err != nil {
		t.Fatalf("native S3 constructor: %s", logging.Redact(err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			assertSafe(t, err.Error())
			t.Fatalf("native MinIO operation: %s", logging.Redact(err.Error()))
		}
	}
	must(store.Create(ctx))
	key := "credentials/" + t.Name() + "/" + suffix
	payload := []byte("credential acceptance payload\x00\xff\n")
	must(store.Put(ctx, key, bytes.NewReader(payload)))
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := store.Delete(cleanup, key); err != nil {
			t.Error("fixture object cleanup failed")
		}
	}()
	reader, err := store.Get(ctx, key, 0, -1)
	must(err)
	data, err := io.ReadAll(reader)
	closeErr := reader.Close()
	must(err)
	must(closeErr)
	if !bytes.Equal(data, payload) {
		t.Fatal("MinIO payload mismatch")
	}
	obj, err := store.Head(ctx, key)
	must(err)
	if obj.Size() != int64(len(payload)) {
		t.Fatal("MinIO object size mismatch")
	}
}
func count(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if want == 0 && os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal("helper invocation evidence missing")
	}
	if strings.Count(string(data), "invoked\n") != want {
		t.Fatal("unexpected helper invocation count")
	}
}

// TestCredentialAcceptance adds YAML-relative sources, one-time resolution,
// warning-only permissions and helper privacy to native real-MinIO acceptance.
// The shared runner is required: no stubbed object-store success, no host secrets.
func TestCredentialAcceptance(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires the shared disposable MinIO: scripts/test-linux.sh unit ./test/credentials")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-access-must-not-be-used")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret-must-not-be-used")
	t.Setenv("AWS_SESSION_TOKEN", "ambient-token-must-not-be-used")

	t.Run("nine_sources_startup_only", func(t *testing.T) {
		for _, a := range []string{"value", "file", "command"} {
			for _, s := range []string{"value", "file", "command"} {
				t.Run(a+"_"+s, func(t *testing.T) {
					capture(t, "json")
					dir := t.TempDir()
					c := load(t, dir, endpoint, source(t, dir, "access", a, access), source(t, dir, "secret", s, secret), "  enabled: false")
					r := resolve(t, c)
					roundTrip(t, r, "first")
					if a == "file" {
						write(t, filepath.Join(dir, "access.secret"), []byte("replaced-invalid-value"), 0600)
					}
					if s == "file" {
						write(t, filepath.Join(dir, "secret.secret"), []byte("replaced-invalid-value"), 0600)
					}
					// A second real native client from the same snapshot still uses startup values.
					roundTrip(t, r, "snapshot")
					if a == "command" {
						count(t, filepath.Join(dir, "access.count"), 1)
					}
					if s == "command" {
						count(t, filepath.Join(dir, "secret.count"), 1)
					}
				})
			}
		}
	})

	t.Run("passphrase_helper_execution", func(t *testing.T) {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprint(enabled), func(t *testing.T) {
				capture(t, "json")
				dir := t.TempDir()
				mode := "fail"
				if enabled {
					mode = "value"
				}
				encryption := fmt.Sprintf("  enabled: %t\n  passphrase: {command: %s}", enabled, encoded(helper(mode, "phrase.count", phrase)))
				c := load(t, dir, endpoint, source(t, dir, "access", "value", access), source(t, dir, "secret", "value", secret), encryption)
				r := resolve(t, c)
				roundTrip(t, r, "passphrase")
				want := 0
				if enabled {
					want = 1
					if r.Passphrase != phrase {
						t.Fatal("enabled passphrase did not resolve")
					}
				} else if r.Passphrase != "" {
					t.Fatal("disabled passphrase resolved")
				}
				count(t, filepath.Join(dir, "phrase.count"), want)
			})
		}
	})

	t.Run("existing_modes_and_owner_warn_only", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Fatal("real foreign-owner fixture requires the shared root Docker runner")
		}
		for _, format := range []string{"text", "json"} {
			t.Run(format, func(t *testing.T) {
				for _, tc := range []struct {
					name    string
					mode    os.FileMode
					foreign bool
					warning string
				}{{"permissive", 0644, false, "continuing without chmod"}, {"foreign_owner", 0600, true, "different owner"}, {"private_read", 0400, false, ""}, {"private_write", 0600, false, ""}} {
					t.Run(tc.name, func(t *testing.T) {
						logs := capture(t, format)
						dir := t.TempDir()
						p := filepath.Join(dir, "credential-file-path-private-marker")
						write(t, p, []byte(access+"\n"), tc.mode)
						if tc.foreign {
							if err := os.Chown(p, 65534, 65534); err != nil {
								t.Fatal("foreign-owner fixture chown failed")
							}
						}
						c := load(t, dir, endpoint, "{file: "+encoded(p)+"}", source(t, dir, "secret", "value", secret), "  enabled: false")
						r := resolve(t, c)
						roundTrip(t, r, "permissions")
						info, err := os.Stat(p)
						if err != nil || info.Mode().Perm() != tc.mode {
							t.Fatal("resolver changed existing file mode")
						}
						if tc.foreign && info.Sys().(*syscall.Stat_t).Uid != 65534 {
							t.Fatal("resolver changed existing file owner")
						}
						text := logs.String()
						if tc.warning != "" && (!strings.Contains(text, tc.warning) || !strings.Contains(text, "s3.access_key")) {
							t.Fatal("required warning missing")
						}
						if tc.warning == "" && (strings.Contains(text, "continuing without chmod") || strings.Contains(text, "different owner")) {
							t.Fatal("private credential unexpectedly warned")
						}
					})
				}
			})
		}
	})

	t.Run("helper_failures_private_no_fallback", func(t *testing.T) {
		for _, format := range []string{"text", "json"} {
			t.Run(format, func(t *testing.T) {
				capture(t, format)
				dir := t.TempDir()
				src := "{command: " + encoded(helper("fail", "failed.count", "argument-private-marker")) + "}"
				c := load(t, dir, endpoint, source(t, dir, "access", "value", access), src, "  enabled: false")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := c.Resolve(ctx, slog.Default())
				if err == nil {
					t.Fatal("failed configured helper fell back to another credential provider")
				}
				assertSafe(t, err.Error())
				slog.Error("expected credential failure", "error", err)
				count(t, filepath.Join(dir, "failed.count"), 1)
			})
		}
	})
}
