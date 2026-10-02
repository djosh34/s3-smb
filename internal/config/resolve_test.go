package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/logging"
)

func TestEnabledPassphraseAndImmediateRegistration(t *testing.T) {
	c := mustConfig(t)
	c.SMB.Password = "smb-distinct-sensitive-marker"
	c.S3.AccessKey = SecretSource{Value: ptr("access-distinct-sensitive-marker")}
	c.S3.SecretKey = SecretSource{Value: ptr("secret-distinct-sensitive-marker")}
	c.S3.SessionToken = "token-distinct-sensitive-marker"
	c.Encryption.Enabled = true
	c.Encryption.Passphrase = SecretSource{Command: helperArgs("echo", "phrase-distinct-sensitive-marker\n")}
	r, err := c.Resolve(context.Background(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if r.Passphrase != "phrase-distinct-sensitive-marker" {
		t.Fatal("passphrase not resolved")
	}
	for _, secret := range []string{c.SMB.Password, r.AccessKey, r.SecretKey, r.SessionToken, r.Passphrase} {
		if logging.Redact(secret) != "[REDACTED]" {
			t.Fatal("resolved secret not registered")
		}
	}
	c.S3.AccessKey = SecretSource{Value: ptr("first-success-before-failure-marker")}
	c.S3.SecretKey = SecretSource{Command: helperArgs("fail")}
	if _, err := c.Resolve(context.Background(), quietLogger()); err == nil {
		t.Fatal("helper failure accepted")
	}
	if logging.Redact(*c.S3.AccessKey.Value) != "[REDACTED]" {
		t.Fatal("first credential registration postponed past later failure")
	}
}
func TestConfigFormattingIsDefensivelyRedacted(t *testing.T) {
	c := mustConfig(t)
	r, err := c.Resolve(context.Background(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{c, r, c.S3.AccessKey} {
		if strings.Contains(fmt.Sprint(value), "marker") {
			t.Fatal("formatting leaks")
		}
		for _, jsonMode := range []bool{false, true} {
			var b bytes.Buffer
			var handler slog.Handler = slog.NewTextHandler(&b, nil)
			if jsonMode {
				handler = slog.NewJSONHandler(&b, nil)
			}
			slog.New(handler).Info("test redaction", "value", value)
			if strings.Contains(b.String(), "marker") {
				t.Fatal("LogValue leaks")
			}
		}
	}
}
