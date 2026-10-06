// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"

	"github.com/djosh34/s3-smb/internal/logging"
)

// Resolved is the startup snapshot of credentials and TLS settings. It prints
// as a placeholder in logs.
type Resolved struct {
	*Config
	TLSConfig    *tls.Config
	AccessKey    string
	SecretKey    string
	SessionToken string
}

func (*Resolved) String() string { return "[redacted resolved configuration]" }

// LogValue keeps the resolved secrets out of logs.
func (*Resolved) LogValue() slog.Value { return slog.StringValue("[redacted resolved configuration]") }

func (*Config) String() string { return "[redacted configuration]" }

// LogValue keeps the configuration out of logs.
func (*Config) LogValue() slog.Value { return slog.StringValue("[redacted configuration]") }

// Resolve runs once at startup, after the logger is configured. It reads the
// secrets and TLS files and warns about loose permissions. A configured CA file
// adds to the system trust pool for this process.
func (c *Config) Resolve(ctx context.Context, logger *slog.Logger) (*Resolved, error) {
	logging.RegisterSecret(c.SMB.Password, c.S3.SessionToken)
	warnExisting(c.path, "configuration", logger)
	warnExisting(c.Storage.StateDir, "storage.state_dir", logger)
	r := &Resolved{Config: c, SessionToken: c.S3.SessionToken}
	var err error
	r.AccessKey, err = c.S3.AccessKey.resolve(ctx, c.secretDir(), "s3.access_key", logger)
	if err != nil {
		return nil, err
	}
	r.SecretKey, err = c.S3.SecretKey.resolve(ctx, c.secretDir(), "s3.secret_key", logger)
	if err != nil {
		return nil, err
	}
	r.TLSConfig, err = c.S3.TLS.load(logger)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (c TLSConfig) load(logger *slog.Logger) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("load process TLS trust roots failed")
	}
	if c.CAFile != "" {
		data, err := readFile(c.CAFile, "s3.tls.ca_file", 1<<20, false, logger)
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("s3.tls.ca_file contains no valid CA certificates")
		}
	}
	result := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if c.ClientCertFile != "" {
		cert, err := readFile(c.ClientCertFile, "s3.tls.client_cert_file", 1<<20, false, logger)
		if err != nil {
			return nil, err
		}
		key, err := readFile(c.ClientKeyFile, "s3.tls.client_key_file", 1<<20, true, logger)
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, errors.New("s3.tls client certificate or private key is invalid or mismatched")
		}
		result.Certificates = []tls.Certificate{pair}
	}
	return result, nil
}
