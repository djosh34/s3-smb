// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, name string) testCA {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, c, c, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func issueCert(t *testing.T, ca testCA, client bool) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if client {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, c, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// mutualTLSServer starts an HTTPS server that trusts only clients of a private
// CA and returns its URL and the client settings.
func mutualTLSServer(t *testing.T) (string, TLSConfig) {
	t.Helper()
	ca := newCA(t, "private test CA")
	serverCert, serverKey := issueCert(t, ca, false)
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	dir := t.TempDir()
	cfg := TLSConfig{CAFile: filepath.Join(dir, "ca.pem"), ClientCertFile: filepath.Join(dir, "client.pem"), ClientKeyFile: filepath.Join(dir, "client-private-marker.key")}
	cert, key := issueCert(t, ca, true)
	for path, data := range map[string][]byte{cfg.CAFile: ca.pem, cfg.ClientCertFile: cert, cfg.ClientKeyFile: key} {
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return server.URL, cfg
}

func requestTLS(t *testing.T, url string, config *tls.Config) error {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func TestTLSPrivateCAAndClientCertificate(t *testing.T) {
	url, cfg := mutualTLSServer(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	loaded, err := cfg.load(logger)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MinVersion != tls.VersionTLS12 || logs.Len() != 0 {
		t.Fatalf("TLS settings or private file warnings: %s", logs.String())
	}
	if err = requestTLS(t, url, loaded); err != nil {
		t.Fatal(err)
	}
}

func TestTLSFileFailuresRedacted(t *testing.T) {
	_, cfg := mutualTLSServer(t)
	_, otherKey := issueCert(t, newCA(t, "other CA"), true)
	mismatched := filepath.Join(t.TempDir(), "other-private-marker.key")
	path := filepath.Join(t.TempDir(), "file-private-marker")
	err := errors.Join(os.WriteFile(mismatched, otherKey, 0o600), os.WriteFile(path, []byte("content-private-marker"), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []TLSConfig{
		{CAFile: path},
		{CAFile: path + "-missing"},
		{ClientCertFile: path, ClientKeyFile: path},
		{ClientCertFile: cfg.ClientCertFile, ClientKeyFile: mismatched},
	} {
		if _, err = c.load(quietLogger()); err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("invalid TLS file accepted or leaked", err)
		}
	}
}
