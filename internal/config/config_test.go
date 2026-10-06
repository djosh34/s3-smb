// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const validYAML = `smb:
  username: backup
  password: smb-password
s3:
  bucket: backups
  access_key: {value: access-marker}
  secret_key: {value: secret-marker}
`

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func loadText(t *testing.T, s string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
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
		"duplicate":         validYAML + "smb: {username: backup, password: private-marker}\n",
		"nested duplicate":  strings.Replace(validYAML, "  bucket: backups", "  bucket: backups\n  bucket: private-marker", 1),
		"trailing document": validYAML + "---\nprivate-marker\n",
		"missing password":  strings.Replace(validYAML, "  password: smb-password\n", "", 1),
		"null password":     strings.Replace(validYAML, "password: smb-password", "password: null", 1),
		"typed encryption":  strings.Replace(validYAML, "  username: backup", "  encryption: invalid\n  username: backup", 1),
		"alias":             "smb: &private-marker {username: backup, password: smb-password}\ns3: *private-marker\n",
		"malformed":         "private-marker: [secret-marker\n",
		"empty":             "",
		"array":             "[private-marker]",
		"null size":         validYAML + "storage: {capacity: null}\n",
		"boolean size":      validYAML + "storage: {capacity: true}\n",
		"binary unit size":  validYAML + "storage: {capacity: 1 MiB}\n",
		"oversize":          strings.Repeat("x", maxConfigBytes+1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadText(t, input)
			if err == nil {
				t.Fatal("accepted invalid configuration")
			}
			if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "secret-marker") {
				t.Fatalf("leaked YAML: %v", err)
			}
		})
	}
}

func TestDefaultsAndRelativePaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	c := mustConfig(t)
	if c.SMB.Listen != "127.0.0.1:445" || c.SMB.Share != "TimeMachine" || !c.SMB.Encryption || c.SMB.ReadOnly || c.Storage.Capacity != 0 || c.S3.PathStyle != nil || c.Logging.Format != "text" {
		t.Fatalf("incorrect defaults: %+v", c)
	}
	if c.Storage.StateDir != filepath.Join(os.Getenv("XDG_DATA_HOME"), "s3-smb") {
		t.Fatal("XDG defaults")
	}
	input := strings.Replace(validYAML, "access_key: {value: access-marker}", "access_key: {file: ./access}", 1)
	input = strings.Replace(input, "secret_key: {value: secret-marker}", "secret_key: {command: [./helper, ./argument]}", 1)
	input = strings.Replace(input, "  bucket: backups", "  bucket: backups\n  path_style: false\n  tls: {ca_file: ./ca, client_cert_file: ./cert, client_key_file: ./key}", 1)
	input = strings.Replace(input, "  username: backup", "  encryption: false\n  username: backup", 1)
	c, err := loadText(t, input+"storage: {state_dir: ./state with spaces}\n")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(c.path)
	if *c.S3.AccessKey.File != filepath.Join(dir, "access") || c.S3.SecretKey.Command[0] != filepath.Join(dir, "helper") || c.S3.SecretKey.Command[1] != "./argument" || c.Storage.StateDir != filepath.Join(dir, "state with spaces") || c.S3.TLS.CAFile != filepath.Join(dir, "ca") || c.S3.TLS.ClientCertFile != filepath.Join(dir, "cert") || c.S3.TLS.ClientKeyFile != filepath.Join(dir, "key") {
		t.Fatal("relative path resolution")
	}
	if c.S3.PathStyle == nil || *c.S3.PathStyle || c.SMB.Encryption {
		t.Fatal("explicit false lost")
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

// Opening a FIFO must not wait for a writer. A regression hangs this test.
func TestLoadRejectsMissingAndNonregularFiles(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing-secret-marker")); err == nil || strings.Contains(err.Error(), "secret-marker") {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fifo); err == nil {
		t.Fatal("FIFO config accepted")
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
		"http tls":                 func(c *Config) { c.S3.Endpoint = "http://localhost:9000"; c.S3.TLS.CAFile = "ca" },
		"port":                     func(c *Config) { c.SMB.Listen = "127.0.0.1:65536" },
		"username":                 func(c *Config) { c.SMB.Username = "" },
		"blank password":           func(c *Config) { c.SMB.Password = "" },
		"bucket":                   func(c *Config) { c.S3.Bucket = "" },
		"share":                    func(c *Config) { c.SMB.Share = "../secret-marker" },
		"share IPC$":               func(c *Config) { c.SMB.Share = "ipc$" },
		"password NUL":             func(c *Config) { c.SMB.Password = "private-marker\x00" },
		"token NUL":                func(c *Config) { c.S3.SessionToken = "secret-marker\x00" },
		"logging":                  func(c *Config) { c.Logging.Level = "private-marker" },
		"state directory NUL":      func(c *Config) { c.Storage.StateDir = "state\x00" },
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

// An unknown key is named in the error. Its value is not.
func TestUnknownFieldIsNamed(t *testing.T) {
	for input, field := range map[string]string{
		validYAML + "colour: {shade: secret-marker}\n":                                                            "colour",
		validYAML + "storage: {size: secret-marker}\n":                                                            "storage.size",
		validYAML + "smb: {username: a, pasword: b}\n":                                                            "smb.pasword",
		strings.Replace(validYAML, "access_key: {value: access-marker}", "access_key: {valeu: secret-marker}", 1): "s3.access_key.valeu",
	} {
		_, err := loadText(t, input)
		if err == nil || err.Error() != "unknown configuration field "+field {
			t.Fatalf("%s: %v", field, err)
		}
	}
}

func TestByteSizes(t *testing.T) {
	for input, want := range map[string]ByteSize{
		"0": 0, "1.5 MB": 1500000, "1 GB": 1000000000, "1 TB": 1000000000000,
		"0.0000001 MB": 1, "0.1 B": 1, "9223372036854775807": 9223372036854775807,
	} {
		c, err := loadText(t, validYAML+fmt.Sprintf("storage: {capacity: %q}\n", input))
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if c.Storage.Capacity != want {
			t.Fatalf("%s: capacity %d, want %d", input, c.Storage.Capacity, want)
		}
	}
	c, err := loadText(t, validYAML+"storage: {capacity: 1234}\n")
	if err != nil || c.Storage.Capacity != 1234 {
		t.Fatalf("unquoted size: %v, %v", c, err)
	}
	for _, input := range []string{"-1", "1 MiB", "1e6", "NaN", "1.2.3 MB", "9223372036854775808", "999999999999 TB"} {
		if _, err := ParseByteSize(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
