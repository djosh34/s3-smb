package config

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `smb:
  username: backup
  password: smb-password
s3:
  bucket: backups
  access_key: {value: access-marker}
  secret_key: {value: secret-marker}
encryption:
  enabled: false
`

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func loadText(t *testing.T, s string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}
func mustConfig(t *testing.T) *Config {
	t.Helper()
	c, err := loadText(t, validYAML)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func ptr(s string) *string { return &s }

func TestStrictYAML(t *testing.T) {
	tests := map[string]string{
		"unknown":           validYAML + "secret-marker-unknown: private-marker\n",
		"nested unknown":    strings.Replace(validYAML, "  bucket: backups", "  private-marker: secret-marker", 1),
		"duplicate":         validYAML + "smb: {username: backup, password: private-marker}\n",
		"nested duplicate":  strings.Replace(validYAML, "  bucket: backups", "  bucket: backups\n  bucket: private-marker", 1),
		"trailing document": validYAML + "---\nprivate-marker\n",
		"missing password":  strings.Replace(validYAML, "  password: smb-password\n", "", 1),
		"null password":     strings.Replace(validYAML, "password: smb-password", "password: null", 1),
		"blank password":    strings.Replace(validYAML, "password: smb-password", "password: \"\"", 1),
		"null enabled":      strings.Replace(validYAML, "enabled: false", "enabled: null", 1),
		"alias":             "smb: &private-marker {username: backup, password: smb-password}\ns3: *private-marker\n",
		"malformed":         "private-marker: [secret-marker\n",
		"empty":             "",
		"array":             "[private-marker]",
		"bad duration":      validYAML + "backup: {interval: private-marker}\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadText(t, input)
			if err == nil {
				t.Fatal("accepted invalid YAML")
			}
			if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "secret-marker") {
				t.Fatalf("leaked YAML: %v", err)
			}
		})
	}
}
func TestFreshDatasetCompressionSelector(t *testing.T) {
	if mustConfig(t).Storage.Compression != nil {
		t.Fatal("omission must adopt the stored codec on recovery")
	}
	for _, codec := range []string{"none", "zstd"} {
		c, err := loadText(t, validYAML+fmt.Sprintf("storage: {compression: %q}\n", codec))
		if err != nil {
			t.Fatalf("supported creation codec %s rejected: %v", codec, err)
		}
		if c.Storage.Compression == nil || *c.Storage.Compression != codec {
			t.Fatal("explicit codec selection lost")
		}
	}
	for _, value := range []string{`""`, `lz4`, `zstd:3`, `invalid`, `null`, `[zstd]`} {
		if _, err := loadText(t, validYAML+"storage: {compression: "+value+"}\n"); err == nil {
			t.Fatalf("accepted unsupported compression selector %s", value)
		}
	}
}

func TestDefaultsPathsAndExplicitness(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	c := mustConfig(t)
	if c.SMB.Listen != "127.0.0.1:445" || c.SMB.Share != "TimeMachine" || c.SMB.Password != "smb-password" || c.SMB.ReadOnly || c.Storage.CacheSize != nil || c.S3.PathStyle != nil || c.Backup.Interval != time.Hour || c.Backup.TrashDays != 14 || c.Logging.Format != "text" {
		t.Fatalf("incorrect defaults")
	}
	if c.Storage.StateDir != filepath.Join(os.Getenv("XDG_DATA_HOME"), "s3-smb") || c.Storage.CacheDir != filepath.Join(os.Getenv("XDG_CACHE_HOME"), "s3-smb") {
		t.Fatal("XDG defaults")
	}
	input := strings.Replace(validYAML, "access_key: {value: access-marker}", "access_key: {file: ./access}", 1)
	input = strings.Replace(input, "secret_key: {value: secret-marker}", "secret_key: {command: [./helper, ./argument]}", 1)
	input = strings.Replace(input, "  bucket: backups", "  bucket: backups\n  tls: {ca_file: ./ca, client_cert_file: ./cert, client_key_file: ./key}", 1)
	c, err := loadText(t, input+"storage: {state_dir: ./state, cache_dir: ./cache, cache_size: 0}\n")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(c.path)
	if *c.S3.AccessKey.File != filepath.Join(dir, "access") || c.S3.SecretKey.Command[0] != filepath.Join(dir, "helper") || c.S3.SecretKey.Command[1] != "./argument" || c.Storage.StateDir != filepath.Join(dir, "state") || c.S3.TLS.CAFile != filepath.Join(dir, "ca") || c.S3.TLS.ClientCertFile != filepath.Join(dir, "cert") || c.S3.TLS.ClientKeyFile != filepath.Join(dir, "key") {
		t.Fatal("relative path resolution")
	}
	if c.Storage.CacheSize == nil || *c.Storage.CacheSize != 0 {
		t.Fatal("explicit zero lost")
	}
	for _, b := range []string{"true", "false"} {
		c, err = loadText(t, strings.Replace(validYAML, "  bucket: backups", "  bucket: backups\n  path_style: "+b, 1))
		if err != nil || c.S3.PathStyle == nil || *c.S3.PathStyle != (b == "true") {
			t.Fatal("path_style explicitness", err)
		}
	}
	c, err = loadText(t, strings.Replace(validYAML, "  enabled: false", "  passphrase: {value: phrase}", 1))
	if err != nil || !c.Encryption.Enabled {
		t.Fatal("encryption must default on", err)
	}
	c, err = loadText(t, strings.Replace(validYAML, "access-marker", "$DO_NOT_INTERPOLATE", 1))
	if err != nil || *c.S3.AccessKey.Value != "$DO_NOT_INTERPOLATE" {
		t.Fatal("implicit interpolation")
	}
}
func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	p, err := DefaultPath()
	if err != nil || p != filepath.Join(home, ".config/s3-smb/config.yaml") {
		t.Fatal(p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "custom"))
	p, err = DefaultPath()
	if err != nil || p != filepath.Join(home, "custom/s3-smb/config.yaml") {
		t.Fatal(p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if _, err = DefaultPath(); err == nil {
		t.Fatal("relative XDG accepted")
	}
}
func TestLoadMissingAndBounded(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing-secret-marker")); err == nil || strings.Contains(err.Error(), "secret-marker") {
		t.Fatal(err)
	}
	if _, err := loadText(t, strings.Repeat("x", maxConfigBytes+1)); err == nil {
		t.Fatal("oversize accepted")
	}
}
func TestValidation(t *testing.T) {
	tests := map[string]func(*Config){
		"missing source":           func(c *Config) { c.S3.AccessKey = SecretSource{} },
		"two sources":              func(c *Config) { c.S3.AccessKey.File = ptr("path") },
		"empty command":            func(c *Config) { c.S3.AccessKey = SecretSource{Command: []string{}} },
		"empty file":               func(c *Config) { c.S3.AccessKey = SecretSource{File: ptr("")} },
		"command NUL":              func(c *Config) { c.S3.AccessKey = SecretSource{Command: []string{"a", "b\x00"}} },
		"bad endpoint":             func(c *Config) { c.S3.Endpoint = "https://private-marker:secret-marker@host" },
		"endpoint query":           func(c *Config) { c.S3.Endpoint = "https://host?secret-marker" },
		"endpoint scheme":          func(c *Config) { c.S3.Endpoint = "host" },
		"tls certificate unpaired": func(c *Config) { c.S3.TLS.ClientCertFile = "secret-marker" },
		"tls key unpaired":         func(c *Config) { c.S3.TLS.ClientKeyFile = "secret-marker" },
		"http tls":                 func(c *Config) { c.S3.Endpoint = "http://localhost:9000"; c.S3.TLS.CAFile = "ca" },
		"interval":                 func(c *Config) { c.Backup.Interval = 0 },
		"trash":                    func(c *Config) { c.Backup.TrashDays = -1 },
		"negative cache bytes":     func(c *Config) { n := ByteSize(-1); c.Storage.CacheSize = &n },
		"port":                     func(c *Config) { c.SMB.Listen = "127.0.0.1:65536" },
		"username":                 func(c *Config) { c.SMB.Username = "" },
		"bucket":                   func(c *Config) { c.S3.Bucket = "" },
		"share":                    func(c *Config) { c.SMB.Share = "../secret-marker" },
		"password NUL":             func(c *Config) { c.SMB.Password = "private-marker\x00" },
		"token NUL":                func(c *Config) { c.S3.SessionToken = "secret-marker\x00" },
		"logging":                  func(c *Config) { c.Logging.Level = "private-marker" },
		"enabled needs passphrase": func(c *Config) { c.Encryption.Enabled = true },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			c := mustConfig(t)
			change(c)
			err := c.validate()
			if err == nil {
				t.Fatal("invalid accepted")
			}
			if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "secret-marker") {
				t.Fatal("leaked", err)
			}
		})
	}
}
func TestDisabledEncryptionDoesNotResolve(t *testing.T) {
	c := mustConfig(t)
	c.Encryption.Passphrase = SecretSource{Command: []string{"/does-not-exist-private-marker"}}
	r, err := c.Resolve(context.Background(), quietLogger())
	if err != nil || r.Passphrase != "" {
		t.Fatal("disabled encryption resolved passphrase", err)
	}
	// Even multiple semantic sources must remain unused when disabled.
	c.Encryption.Passphrase.File = ptr("/also-not-present")
	if _, err := c.Resolve(context.Background(), quietLogger()); err != nil {
		t.Fatal(err)
	}
}
func TestCredentialCombinationsAndStartupSnapshot(t *testing.T) {
	source := func(kind, value string) SecretSource {
		switch kind {
		case "value":
			return SecretSource{Value: ptr(value)}
		case "file":
			p := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(p, []byte(value+"\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			return SecretSource{File: &p}
		default:
			return SecretSource{Command: helperArgs("echo", value+"\n")}
		}
	}
	for _, kind := range []string{"value", "file", "command"} {
		t.Run(kind, func(t *testing.T) {
			c := mustConfig(t)
			c.S3.AccessKey = source(kind, "access-private-marker")
			c.S3.SecretKey = source(kind, "secret-private-marker")
			c.S3.SessionToken = "token-private-marker"
			r, err := c.Resolve(context.Background(), quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			if r.AccessKey != "access-private-marker" || r.SecretKey != "secret-private-marker" || r.SessionToken != "token-private-marker" {
				t.Fatal("wrong resolved credentials")
			}
			if c.S3.AccessKey.File != nil {
				if err := os.WriteFile(*c.S3.AccessKey.File, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if r.AccessKey != "access-private-marker" {
				t.Fatal("snapshot changed")
			}
		})
	}
}
func TestSecretValueRules(t *testing.T) {
	for _, tt := range []struct {
		input, want string
		bad         bool
	}{{"", "", true}, {"\n", "", true}, {"\r\n", "", true}, {"a\x00b", "", true}, {" a \n", " a ", false}, {"a\r\n", "a", false}, {"a\n\n", "a\n", false}, {"a\r", "a\r", false}, {strings.Repeat("a", MaxSecretBytes+1), "", true}} {
		s := SecretSource{Value: ptr(tt.input)}
		v, err := s.resolve(context.Background(), ".", "test", quietLogger())
		if (err != nil) != tt.bad || (!tt.bad && v != tt.want) {
			t.Fatalf("value rules: got %q, error %v", v, err)
		}
	}
}
func TestSizeDecimalOmittedZeroSmallPositive(t *testing.T) {
	cases := map[string]int64{"0": 0, "0 MB": 0, "1 MB": 1000000, "1 GB": 1000000000, "1.5 MB": 1500000, "0.000001 MB": 1, "0.0000001 MB": 1, "0.1 B": 1, "1 KB": 1000, "1 TB": 1000000000000, "9223372036854775807": 9223372036854775807}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got, err := ParseByteSize(input)
			if err != nil || int64(got) != want {
				t.Fatal(got, err, want)
			}
			c, err := loadText(t, validYAML+fmt.Sprintf("storage: {cache_size: %q}\n", input))
			if err != nil || c.Storage.CacheSize == nil || int64(*c.Storage.CacheSize) != want {
				t.Fatal("YAML capacity", err)
			}
		})
	}
	for _, input := range []string{"-1", "1 MiB", "1e6", "NaN", "1.2.3 MB", "9223372036854775808", "9223372036854775807.1", "999999999999 TB"} {
		if _, err := ParseByteSize(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	if mustConfig(t).Storage.CacheSize != nil {
		t.Fatal("omitted capacity not nil")
	}
}
