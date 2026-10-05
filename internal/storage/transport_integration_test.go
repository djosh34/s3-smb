// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/logging"
)

// transportSMBMarker is the SMB password. It must not show up in the logs.
var transportSMBMarker = "transport-smb-password-marker"

const (
	transportBucket = "transport-test"
	// scripts/check.sh resolves this name and its bucket subdomain to the runner.
	transportHost = "transport.test"
)

// TestTransportAcceptance runs the config resolver and the S3 client against
// MinIO through a local TLS proxy. The proxy terminates TLS and records each
// request. It leaves the signed Host and path unchanged.
func TestTransportAcceptance(t *testing.T) {
	upstream := minioEndpoint(t)
	ca := transportCA(t)
	wrongCA := transportCA(t)
	serverCert, serverKey := ca.issue(t, false)
	clientCert, clientKey := ca.issue(t, true)
	caFile := transportFile(t, "ca.pem", ca.pem)
	wrongCAFile := transportFile(t, "wrong-ca.pem", wrongCA.pem)
	certFile := transportFile(t, "client.pem", clientCert)
	keyFile := transportFile(t, "client-key.pem", clientKey)
	serverPair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name              string
		pathStyle, mutual bool
	}{
		{"path_style_true/private_ca", true, false},
		{"path_style_false/mutual_tls", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := transportProxy(t, upstream, serverPair, ca.cert, tc.mutual)
			cfg := transportConfig(t, proxy.endpoint, tc.pathStyle)
			cfg.S3.TLS.CAFile = caFile
			if tc.mutual {
				cfg.S3.TLS.ClientCertFile, cfg.S3.TLS.ClientKeyFile = certFile, keyFile
			}
			store := transportOpen(t, cfg)
			transportRoundTrip(t, store, "accepted/"+tc.name)
			proxy.assertRequests(t, tc.pathStyle, tc.mutual, "")
		})
	}

	t.Run("wrong_ca", func(t *testing.T) {
		proxy := transportProxy(t, upstream, serverPair, ca.cert, false)
		cfg := transportConfig(t, proxy.endpoint, true)
		cfg.S3.TLS.CAFile = wrongCAFile
		store := transportOpen(t, cfg)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		_, err := store.Head(ctx, "accepted/path_style_true/private_ca")
		if err == nil {
			t.Fatal("untrusted TLS connection succeeded")
		}
		access, secret := minioCredentials()
		transportNoSecrets(t, err.Error(), access, secret)
		if got := proxy.snapshot(); len(got) != 0 {
			t.Fatal("TLS authentication failure reached the S3 HTTP handler")
		}
	})

	t.Run("static_session_token", func(t *testing.T) {
		// MinIO validates a real signed STS token. The fixture gets one; the
		// application only receives static configured values.
		access, secret, token := transportSTS(t, upstream)
		proxy := transportProxy(t, upstream, serverPair, ca.cert, false)
		proxy.expectedToken = token
		cfg := transportConfig(t, proxy.endpoint, false)
		cfg.S3.AccessKey = config.SecretSource{Value: &access}
		cfg.S3.SecretKey = config.SecretSource{Value: &secret}
		cfg.S3.SessionToken, cfg.S3.TLS.CAFile = token, caFile
		// Configured credentials must win over the environment.
		t.Setenv("AWS_ACCESS_KEY_ID", "ambient-access-must-not-be-used")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret-must-not-be-used")
		t.Setenv("AWS_SESSION_TOKEN", "ambient-token-must-not-be-used")
		transportRoundTrip(t, transportOpen(t, cfg), "accepted/static-token")
		proxy.assertRequests(t, false, false, token)
		for _, r := range proxy.snapshot() {
			if r.method == http.MethodPost {
				t.Fatal("S3 client tried to acquire or refresh credentials")
			}
		}
	})
}

func transportConfig(t *testing.T, endpoint string, pathStyle bool) *config.Config {
	t.Helper()
	access, secret := minioCredentials()
	return &config.Config{
		SMB:     config.SMBConfig{Listen: "127.0.0.1:445", Share: "transport", Username: "transport", Password: transportSMBMarker},
		Storage: config.StorageConfig{StateDir: t.TempDir(), CacheDir: t.TempDir()},
		S3: config.S3Config{
			Bucket: transportBucket, Region: "us-east-1", Endpoint: endpoint, PathStyle: &pathStyle,
			AccessKey: config.SecretSource{Value: &access}, SecretKey: config.SecretSource{Value: &secret},
		},
		Backup:  config.BackupConfig{Interval: time.Hour, TrashDays: 14},
		Logging: config.LoggingConfig{Format: "json", Level: "debug"},
	}
}

// transportOpen resolves cfg like startup does, including the CA and client
// key files, and checks at cleanup that the logs hold no secrets.
func transportOpen(t *testing.T, cfg *config.Config) object.ObjectStorage {
	t.Helper()
	var logs transportBuffer
	logging.Install(&logs)
	if err := logging.Configure("json", "debug"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		logging.Install(os.Stderr)
		data := logs.String()
		access, secret := minioCredentials()
		secrets := []string{access, secret, cfg.S3.SessionToken, cfg.SMB.Password}
		for _, source := range []config.SecretSource{cfg.S3.AccessKey, cfg.S3.SecretKey} {
			if source.Value != nil {
				secrets = append(secrets, *source.Value)
			}
		}
		transportNoSecrets(t, data, secrets...)
		for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
			if line != "" && !json.Valid([]byte(line)) {
				t.Error("log output contained non-JSON lines")
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resolved, err := cfg.Resolve(ctx, slog.Default())
	if err != nil {
		t.Fatalf("resolve transport config: %s", logging.Redact(err.Error()))
	}
	store, err := OpenS3(resolved)
	if err != nil {
		t.Fatalf("construct S3 client: %s", logging.Redact(err.Error()))
	}
	if closer, ok := store.(io.Closer); ok {
		t.Cleanup(func() {
			if closeErr := closer.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	return store
}

func transportRoundTrip(t *testing.T, store object.ObjectStorage, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	must := func(op string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("S3 %s: %s", op, logging.Redact(err.Error()))
		}
	}
	must("Create", store.Create(ctx))
	payload := []byte("S3 transport acceptance\x00\xff\n")
	must("Put", store.Put(ctx, key, bytes.NewReader(payload)))
	r, err := store.Get(ctx, key, 0, -1)
	must("Get", err)
	data, err := io.ReadAll(r)
	must("Read", errors.Join(err, r.Close()))
	if !bytes.Equal(data, payload) {
		t.Fatal("MinIO round trip changed the payload")
	}
	obj, err := store.Head(ctx, key)
	must("Head", err)
	if obj.Size() != int64(len(payload)) {
		t.Fatal("MinIO HEAD returned the wrong size")
	}
	objs, _, _, err := store.List(ctx, key, "", "", "", 100, true)
	must("List", err)
	if len(objs) != 1 || objs[0].Key() != key {
		t.Fatal("MinIO LIST did not return the written object")
	}
	must("Delete", store.Delete(ctx, key))
	if _, err = store.Head(ctx, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("HEAD of a deleted object did not report not-exist")
	}
	missing, err := store.Get(ctx, key, 0, -1)
	if missing != nil {
		must("Close", missing.Close())
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("GET of a deleted object did not report not-exist")
	}
}

type transportRequest struct {
	method, host, path string
	status             int
	tlsVersion         uint16
	clientCert         bool
	tokenOK            bool
}

type transportObserver struct {
	endpoint      string
	expectedToken string
	requests      []transportRequest
	mu            sync.Mutex
}

func (p *transportObserver) snapshot() []transportRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]transportRequest(nil), p.requests...)
}

func (p *transportObserver) assertRequests(t *testing.T, pathStyle, mutual bool, token string) {
	t.Helper()
	endpoint, err := url.Parse(p.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	host := endpoint.Host
	prefix := "/"
	if pathStyle {
		prefix = "/" + transportBucket + "/"
	} else {
		host = transportBucket + "." + host
	}
	methods := map[string]bool{}
	for _, r := range p.snapshot() {
		if r.host != host {
			t.Errorf("signed request Host = %q, want %q", r.host, host)
		}
		if r.path != strings.TrimSuffix(prefix, "/") && !strings.HasPrefix(r.path, prefix) {
			t.Errorf("request path %q does not use addressing prefix %q", r.path, prefix)
		}
		if !pathStyle && strings.HasPrefix(r.path, "/"+transportBucket+"/") {
			t.Error("virtual-host mode fell back to path style")
		}
		if r.tlsVersion < tls.VersionTLS12 {
			t.Error("request did not use TLS 1.2 or later")
		}
		if r.clientCert != mutual {
			t.Error("unexpected client certificate state")
		}
		if token != "" && !r.tokenOK {
			t.Error("request omitted or changed the session token")
		}
		if r.status >= 200 && r.status < 300 {
			methods[r.method] = true
		}
	}
	for _, method := range []string{"PUT", "GET", "HEAD", "DELETE"} {
		if !methods[method] {
			t.Errorf("no successful MinIO %s response observed", method)
		}
	}
}

func transportProxy(t *testing.T, upstream *url.URL, pair tls.Certificate, ca *x509.Certificate, mutual bool) *transportObserver {
	t.Helper()
	p := &transportObserver{}
	backend, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("default HTTP transport has an unexpected type")
	}
	backend = backend.Clone()
	backend.Proxy = nil
	t.Cleanup(backend.CloseIdleConnections)
	proxy := &httputil.ReverseProxy{
		// Keep the signed Host header; only the connection goes to MinIO.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = upstream.Scheme
			r.Out.URL.Host = upstream.Host
			r.Out.Host = r.In.Host
		},
		Transport: backend, ErrorLog: log.New(io.Discard, "", 0),
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := transportRequest{
			method: r.Method, host: r.Host, path: r.URL.Path,
			tokenOK: r.Header.Get("X-Amz-Security-Token") == p.expectedToken,
		}
		if r.TLS != nil {
			observed.tlsVersion = r.TLS.Version
			observed.clientCert = len(r.TLS.VerifiedChains) > 0
		}
		recorder := &transportStatusWriter{ResponseWriter: w}
		proxy.ServeHTTP(recorder, r)
		observed.status = recorder.status
		p.mu.Lock()
		p.requests = append(p.requests, observed)
		p.mu.Unlock()
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	if mutual {
		server.TLS.ClientAuth = tls.RequireAndVerifyClientCert
		server.TLS.ClientCAs = x509.NewCertPool()
		server.TLS.ClientCAs.AddCert(ca)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p.endpoint = "https://" + net.JoinHostPort(transportHost, port)
	return p
}

type transportStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *transportStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *transportStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

type transportAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func transportCA(t *testing.T) *transportAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: transportSerial(t), Subject: pkix.Name{CommonName: "disposable transport test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &transportAuthority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *transportAuthority) issue(t *testing.T, client bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: transportSerial(t), Subject: pkix.Name{CommonName: "disposable transport leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		DNSNames: []string{transportHost, "*." + transportHost}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if client {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		template.DNSNames = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
}

func transportSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	return n.Add(n, big.NewInt(1))
}

func transportFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type transportBuffer struct {
	data bytes.Buffer
	mu   sync.Mutex
}

func (b *transportBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *transportBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func transportNoSecrets(t *testing.T, data string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(data, secret) {
			t.Error("a secret leaked into error or log output")
		}
	}
}

// transportSTS gets temporary credentials from MinIO's STS endpoint.
func transportSTS(t *testing.T, upstream *url.URL) (access, secret, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	body := "Action=AssumeRole&Version=2011-06-15&DurationSeconds=900"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.JoinPath("/").String(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sum := sha256.Sum256([]byte(body))
	rootAccess, rootSecret := minioCredentials()
	err = v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: rootAccess, SecretAccessKey: rootSecret}, request, hex.EncodeToString(sum[:]), "sts", "us-east-1", time.Now())
	if err != nil {
		t.Fatal("sign STS request")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("request MinIO STS credentials")
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("MinIO STS returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Result struct {
			Credentials struct {
				Access string `xml:"AccessKeyId"`
				Secret string `xml:"SecretAccessKey"`
				Token  string `xml:"SessionToken"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleResult"`
	}
	if err = xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		t.Fatal("decode MinIO STS response")
	}
	c := result.Result.Credentials
	if c.Access == "" || c.Secret == "" || c.Token == "" {
		t.Fatal("MinIO STS returned incomplete credentials")
	}
	return c.Access, c.Secret, c.Token
}
