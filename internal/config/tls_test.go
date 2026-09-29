package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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
func issueCert(t *testing.T, ca testCA, client bool) ([]byte, []byte) {
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
func tlsFixture(t *testing.T, mutual bool) (string, TLSConfig) {
	t.Helper()
	ca := newCA(t, "private test CA")
	serverCert, serverKey := issueCert(t, ca, false)
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	if mutual {
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		server.TLS.ClientCAs = pool
		server.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	dir := t.TempDir()
	cfg := TLSConfig{CAFile: filepath.Join(dir, "ca.pem")}
	if err := os.WriteFile(cfg.CAFile, ca.pem, 0644); err != nil {
		t.Fatal(err)
	}
	if mutual {
		cert, key := issueCert(t, ca, true)
		cfg.ClientCertFile = filepath.Join(dir, "client.pem")
		cfg.ClientKeyFile = filepath.Join(dir, "client-private-marker.key")
		if err := os.WriteFile(cfg.ClientCertFile, cert, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.ClientKeyFile, key, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return server.URL, cfg
}
func requestTLS(url string, config *tls.Config) error {
	tr := &http.Transport{TLSClientConfig: config}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err == nil {
		response.Body.Close()
	}
	return err
}
func TestTLSVerifiedCustomCAAndHostname(t *testing.T) {
	url, cfg := tlsFixture(t, false)
	var logs bytes.Buffer
	loaded, err := cfg.load(slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InsecureSkipVerify || loaded.MinVersion != tls.VersionTLS12 || logs.Len() != 0 {
		t.Fatal("TLS verification or public certificate warning")
	}
	if err := requestTLS(url, loaded); err != nil {
		t.Fatal("private CA trust", err)
	}
	loaded.ServerName = "incorrect.example"
	if err := requestTLS(url, loaded); err == nil {
		t.Fatal("incorrect hostname accepted")
	}
	other := newCA(t, "wrong CA")
	if err := os.WriteFile(cfg.CAFile, other.pem, 0644); err != nil {
		t.Fatal(err)
	}
	wrong, err := cfg.load(quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := requestTLS(url, wrong); err == nil {
		t.Fatal("wrong CA accepted")
	}
	loaded.ServerName = ""
	if err := requestTLS(url, loaded); err != nil {
		t.Fatal("replacing CA file altered existing startup snapshot", err)
	}
	system, err := (TLSConfig{}).load(quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := requestTLS(url, system); err == nil {
		t.Fatal("private CA leaked into global/OS roots")
	}
}
func TestTLSMutualAuthenticationAndPrivateKeyWarnings(t *testing.T) {
	url, cfg := tlsFixture(t, true)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	loaded, err := cfg.load(logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := requestTLS(url, loaded); err != nil {
		t.Fatal("mTLS", err)
	}
	if logs.Len() != 0 {
		t.Fatal("public cert or private mode warned", logs.String())
	}
	without := loaded.Clone()
	without.Certificates = nil
	if err := requestTLS(url, without); err == nil {
		t.Fatal("server accepted missing client cert")
	}
	if err := os.Chmod(cfg.ClientKeyFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.load(logger); err != nil {
		t.Fatal("readable key rejected", err)
	}
	if !strings.Contains(logs.String(), "s3.tls.client_key_file") || strings.Contains(logs.String(), "private-marker") || strings.Contains(logs.String(), "s3.tls.client_cert_file") || strings.Contains(logs.String(), "s3.tls.ca_file") {
		t.Fatal("wrong warnings", logs.String())
	}
	other := newCA(t, "other CA")
	_, otherKey := issueCert(t, other, true)
	if err := os.WriteFile(cfg.ClientKeyFile, otherKey, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.load(quietLogger()); err == nil || strings.Contains(err.Error(), "private-marker") {
		t.Fatal("mismatched key accepted/leaked", err)
	}
	if err := requestTLS(url, loaded); err != nil {
		t.Fatal("replacing key altered snapshot", err)
	}
}
func TestTLSFileFailuresRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file-private-marker")
	if err := os.WriteFile(path, []byte("content-private-marker"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []TLSConfig{{CAFile: path}, {CAFile: path + "-missing"}, {ClientCertFile: path, ClientKeyFile: path}} {
		_, err := cfg.load(quietLogger())
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("TLS parser leaked", err)
		}
	}
}
