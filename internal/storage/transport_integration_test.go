// SPDX-License-Identifier: AGPL-3.0-only
package storage_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/logging"
	"github.com/djosh34/s3-smb/internal/storage"
)

const transportBucket = "transport-test"
const transportHost = "transport.minio"
const transportAccess = "s3smb-test-access"
const transportSecret = "s3smb-test-secret-only"

// TestTransportAcceptance uses the production config resolver and native S3
// client against the shared, source-pinned MinIO. The local proxy terminates TLS
// and observes requests, but never fabricates successful S3 responses or changes
// their signed Host/path. See test/transport/README.md for the precise boundary.
func TestTransportAcceptance(t *testing.T) {
	if os.Getenv("S3_SMB_TRANSPORT") != "1" {
		t.Skip("run scripts/test-linux.sh e2e for native S3 transport acceptance")
	}
	upstream, err := url.Parse(os.Getenv("S3_SMB_E2E_ENDPOINT"))
	if err != nil || upstream.Host == "" || upstream.Scheme != "http" {
		t.Fatal("transport acceptance requires the shared disposable HTTP MinIO endpoint")
	}
	transportWaitMinIO(t, upstream)
	ca := transportCA(t)
	wrongCA := transportCA(t)
	serverCert, _ := ca.issue(t, false)
	clientCert, clientKey := ca.issue(t, true)
	caFile := transportFile(t, "ca.pem", ca.pem)
	wrongCAFile := transportFile(t, "wrong-ca.pem", wrongCA.pem)
	certFile := transportFile(t, "client.pem", clientCert)
	keyFile := transportFile(t, "client-key.pem", clientKey)
	serverPair, err := tls.X509KeyPair(serverCert, ca.serverKey)
	if err != nil {
		t.Fatal(err)
	}

	for _, pathStyle := range []bool{true, false} {
		style := "path_style_false"
		if pathStyle {
			style = "path_style_true"
		}
		t.Run(style, func(t *testing.T) {
			for _, mutual := range []bool{false, true} {
				name := "private_ca"
				if mutual {
					name = "mutual_tls"
				}
				t.Run(name, func(t *testing.T) {
					proxy := transportProxy(t, upstream, serverPair, ca.cert, mutual)
					cfg := transportConfig(t, proxy.endpoint, pathStyle)
					cfg.S3.TLS.CAFile = caFile
					if mutual {
						cfg.S3.TLS.ClientCertFile, cfg.S3.TLS.ClientKeyFile = certFile, keyFile
					}
					store := transportOpen(t, cfg)
					transportRoundTrip(t, store, "accepted/"+style+"/"+name)
					proxy.assertRequests(t, pathStyle, true, mutual, "")
				})
			}
		})
	}

	for _, tc := range []struct {
		name   string
		ca     string
		mutual bool
	}{
		{"wrong_ca", wrongCAFile, false},
		{"untrusted_private_ca", "", false},
		{"missing_client_certificate", caFile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := transportProxy(t, upstream, serverPair, ca.cert, tc.mutual)
			cfg := transportConfig(t, proxy.endpoint, true)
			cfg.S3.TLS.CAFile = tc.ca
			store := transportOpen(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			_, err := store.Head(ctx, "accepted/path_style_true/private_ca")
			if err == nil {
				t.Fatal("untrusted TLS connection unexpectedly succeeded")
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("TLS failure exceeded caller deadline")
			}
			transportNoSecrets(t, err.Error(), transportAccess, transportSecret)
			if got := proxy.snapshot(); len(got) != 0 {
				t.Fatal("TLS authentication failure reached the S3 HTTP handler")
			}
			if proxy.plain.Load() != 0 {
				t.Fatal("client attempted plaintext downgrade after TLS failure")
			}
			t.Log("native S3 rejected TLS; no HTTP request, no plaintext retry")
		})
	}

	t.Run("ca_replacement_requires_new_snapshot", func(t *testing.T) {
		proxy := transportProxy(t, upstream, serverPair, ca.cert, false)
		replacement := transportFile(t, "replace-ca.pem", wrongCA.pem)
		cfg := transportConfig(t, proxy.endpoint, true)
		cfg.S3.TLS.CAFile = replacement
		old := transportOpen(t, cfg)
		if err := os.WriteFile(replacement, ca.pem, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := old.Head(ctx, "accepted/path_style_true/private_ca"); err == nil {
			t.Fatal("already resolved client unexpectedly reloaded trust roots")
		}
		fresh := transportOpen(t, cfg)
		transportRoundTrip(t, fresh, "accepted/replaced-ca")
		proxy.assertRequests(t, true, true, false, "")
	})

	t.Run("explicit_http", func(t *testing.T) {
		cfg := transportConfig(t, upstream.String(), true)
		transportRoundTrip(t, transportOpen(t, cfg), "accepted/explicit-http")
	})

	t.Run("static_session_token", func(t *testing.T) {
		// MinIO validates a real signed STS token, not an invented header. Acquire it
		// once as fixture setup; the application receives only explicit static values.
		access, secret, token := transportSTS(t, upstream)
		proxy := transportProxy(t, upstream, serverPair, ca.cert, false)
		proxy.expectedToken = token
		cfg := transportConfig(t, proxy.endpoint, false)
		cfg.S3.AccessKey = config.SecretSource{Value: &access}
		cfg.S3.SecretKey = config.SecretSource{Value: &secret}
		cfg.S3.SessionToken, cfg.S3.TLS.CAFile = token, caFile
		// Poison ambient providers: configured credentials must be authoritative.
		t.Setenv("AWS_ACCESS_KEY_ID", "ambient-access-must-not-be-used")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret-must-not-be-used")
		t.Setenv("AWS_SESSION_TOKEN", "ambient-token-must-not-be-used")
		transportRoundTrip(t, transportOpen(t, cfg), "accepted/static-token")
		proxy.assertRequests(t, false, true, false, token)
		for _, r := range proxy.snapshot() {
			if r.method == http.MethodPost {
				t.Fatal("native client attempted credential acquisition/refresh")
			}
		}
		t.Log("MinIO accepted explicit startup token on signed native PUT/GET/HEAD/LIST/DELETE; no refresh")
	})

	t.Run("invalid_static_credentials", func(t *testing.T) {
		proxy := transportProxy(t, upstream, serverPair, ca.cert, false)
		cfg := transportConfig(t, proxy.endpoint, true)
		wrong := "transport-invalid-secret-marker"
		cfg.S3.SecretKey = config.SecretSource{Value: &wrong}
		cfg.S3.TLS.CAFile = caFile
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := transportOpen(t, cfg).Head(ctx, "accepted/path_style_true/private_ca")
		if err == nil {
			t.Fatal("MinIO accepted an invalid signing secret")
		}
		transportNoSecrets(t, err.Error(), transportAccess, wrong)
		requests := proxy.snapshot()
		if len(requests) != 1 || requests[0].status != http.StatusForbidden {
			t.Fatalf("want one real MinIO 403, got %d requests", len(requests))
		}
	})

	t.Run("caller_deadline", func(t *testing.T) {
		proxy := transportProxy(t, upstream, serverPair, ca.cert, false)
		proxy.stall.Store(true)
		cfg := transportConfig(t, proxy.endpoint, true)
		cfg.S3.TLS.CAFile = caFile
		store := transportOpen(t, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := store.Head(ctx, "deadline-probe")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("native S3 did not preserve caller deadline error")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("native S3 ignored bounded caller deadline")
		}
		transportNoSecrets(t, err.Error(), transportAccess, transportSecret)
		t.Log("stalled TLS HTTP response canceled by 300ms caller deadline")
	})
}

func transportConfig(t *testing.T, endpoint string, pathStyle bool) *config.Config {
	t.Helper()
	password, access, secret := "transport-smb-password-marker", transportAccess, transportSecret
	return &config.Config{
		SMB:     config.SMBConfig{Listen: "127.0.0.1:445", Share: "transport", Username: "transport", Password: &password},
		Storage: config.StorageConfig{StateDir: t.TempDir(), CacheDir: t.TempDir()},
		S3: config.S3Config{Bucket: transportBucket, Region: "us-east-1", Endpoint: endpoint, PathStyle: &pathStyle,
			AccessKey: config.SecretSource{Value: &access}, SecretKey: config.SecretSource{Value: &secret}},
		Backup:  config.BackupConfig{Interval: time.Hour, TrashDays: 14},
		Logging: config.LoggingConfig{Format: "json", Level: "debug"},
	}
}

// Use the same config resolver as startup, including CA/client-key file loading.
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
		secrets := []string{transportAccess, transportSecret, cfg.S3.SessionToken, *cfg.SMB.Password}
		if cfg.S3.AccessKey.Value != nil {
			secrets = append(secrets, *cfg.S3.AccessKey.Value)
		}
		if cfg.S3.SecretKey.Value != nil {
			secrets = append(secrets, *cfg.S3.SecretKey.Value)
		}
		transportNoSecrets(t, data, secrets...)
		for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
			if line != "" && !json.Valid([]byte(line)) {
				t.Error("native diagnostic stream contained non-JSON output")
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resolved, err := cfg.Resolve(ctx, slog.Default())
	if err != nil {
		t.Fatalf("resolve transport config: %s", logging.Redact(err.Error()))
	}
	store, err := storage.OpenS3(resolved)
	if err != nil {
		t.Fatalf("construct native S3: %s", logging.Redact(err.Error()))
	}
	t.Cleanup(func() {
		if closer, ok := store.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	return store
}

func transportRoundTrip(t *testing.T, store object.ObjectStorage, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	must := func(op string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("native S3 %s: %s", op, logging.Redact(err.Error()))
		}
	}
	must("Create", store.Create(ctx))
	payload := []byte("native S3 transport acceptance\x00\xff\n")
	must("Put", store.Put(ctx, key, bytes.NewReader(payload)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = store.Delete(ctx, key)
	})
	r, err := store.Get(ctx, key, 0, -1)
	must("Get", err)
	data, err := io.ReadAll(r)
	closeErr := r.Close()
	must("Read", err)
	must("Close", closeErr)
	if !bytes.Equal(data, payload) {
		t.Fatal("real MinIO round trip changed payload")
	}
	obj, err := store.Head(ctx, key)
	must("Head", err)
	if obj.Size() != int64(len(payload)) {
		t.Fatal("real MinIO HEAD returned wrong size")
	}
	objs, _, _, err := store.List(ctx, key, "", "", "", 100, true)
	must("List", err)
	if len(objs) != 1 || objs[0].Key() != key {
		t.Fatal("real MinIO LIST did not return written object")
	}
	must("Delete", store.Delete(ctx, key))
	_, err = store.Head(ctx, key)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted object still exists or HEAD did not report not-exist")
	}
	t.Log("native Create/Put/Get/Head/List/Delete verified against MinIO with exact binary payload")
}

type transportRequest struct {
	method, host, path  string
	tlsVersion          uint16
	clientCert, tokenOK bool
	status              int
}
type transportObserver struct {
	endpoint      string
	mu            sync.Mutex
	requests      []transportRequest
	plain         atomic.Int64
	stall         atomic.Bool
	expectedToken string
}

func (p *transportObserver) snapshot() []transportRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]transportRequest(nil), p.requests...)
}
func (p *transportObserver) assertRequests(t *testing.T, pathStyle, secure, mutual bool, token string) {
	t.Helper()
	endpoint, _ := url.Parse(p.endpoint)
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
			t.Error("virtual-host mode silently fell back to path-style")
		}
		if secure && r.tlsVersion < tls.VersionTLS12 {
			t.Error("native request did not use verified TLS 1.2+")
		}
		if r.clientCert != mutual {
			t.Error("unexpected authenticated client-certificate state")
		}
		if token != "" && !r.tokenOK {
			t.Error("native request omitted/changed explicit session token")
		}
		if r.status >= 200 && r.status < 300 {
			methods[r.method] = true
		}
	}
	for _, method := range []string{"PUT", "GET", "HEAD", "DELETE"} {
		if !methods[method] {
			t.Errorf("no successful real MinIO %s response observed", method)
		}
	}
	if p.plain.Load() != 0 {
		t.Error("native client attempted plaintext on TLS endpoint")
	}
	t.Logf("observed signed Host=%s, path prefix=%s, TLS>=1.2, mutual=%t", host, prefix, mutual)
}

func transportProxy(t *testing.T, upstream *url.URL, pair tls.Certificate, ca *x509.Certificate, mutual bool) *transportObserver {
	t.Helper()
	p := &transportObserver{}
	backend := http.DefaultTransport.(*http.Transport).Clone()
	backend.Proxy = nil
	t.Cleanup(backend.CloseIdleConnections)
	proxy := &httputil.ReverseProxy{
		Director:  func(r *http.Request) { r.URL.Scheme = upstream.Scheme; r.URL.Host = upstream.Host },
		Transport: backend, ErrorLog: log.New(io.Discard, "", 0),
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.stall.Load() {
			<-r.Context().Done()
			return
		}
		observed := transportRequest{method: r.Method, host: r.Host, path: r.URL.Path,
			tokenOK: r.Header.Get("X-Amz-Security-Token") == p.expectedToken}
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
	server.Listener = &transportSniffListener{Listener: server.Listener, plain: &p.plain}
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
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}

type transportSniffListener struct {
	net.Listener
	plain *atomic.Int64
}

func (l *transportSniffListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &transportSniffConn{Conn: c, plain: l.plain}, nil
}

type transportSniffConn struct {
	net.Conn
	plain *atomic.Int64
	seen  bool
}

func (c *transportSniffConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && !c.seen {
		c.seen = true
		if b[0] != 22 {
			c.plain.Add(1)
		}
	}
	return n, err
}

type transportAuthority struct {
	cert           *x509.Certificate
	key            *ecdsa.PrivateKey
	pem, serverKey []byte
}

func transportCA(t *testing.T) *transportAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: transportSerial(t), Subject: pkix.Name{CommonName: "disposable transport test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
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
func (ca *transportAuthority) issue(t *testing.T, client bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: transportSerial(t), Subject: pkix.Name{CommonName: "disposable transport leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		DNSNames: []string{transportHost, "*." + transportHost}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
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
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
	if !client {
		ca.serverKey = keyPEM
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM
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
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type transportBuffer struct {
	sync.Mutex
	data bytes.Buffer
}

func (b *transportBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.data.Write(p)
}
func (b *transportBuffer) String() string { b.Lock(); defer b.Unlock(); return b.data.String() }
func transportNoSecrets(t *testing.T, data string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(data, secret) {
			t.Error("secret marker leaked in native error/diagnostic output")
		}
	}
}

func transportWaitMinIO(t *testing.T, u *url.URL) {
	t.Helper()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(u.String() + "/minio/health/ready")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("shared pinned MinIO did not become ready in 30s")
}

func transportSTS(t *testing.T, upstream *url.URL) (string, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := "Action=AssumeRole&Version=2011-06-15&DurationSeconds=900"
	request, err := http.NewRequestWithContext(ctx, "POST", upstream.String()+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sum := sha256.Sum256([]byte(body))
	err = v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: transportAccess, SecretAccessKey: transportSecret}, request, hex.EncodeToString(sum[:]), "sts", "us-east-1", time.Now())
	if err != nil {
		t.Fatal("sign disposable STS fixture request")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("request disposable MinIO STS credentials")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("MinIO STS fixture returned HTTP %d", response.StatusCode)
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
	if err := xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		t.Fatal("decode disposable STS fixture response")
	}
	c := result.Result.Credentials
	if c.Access == "" || c.Secret == "" || c.Token == "" {
		t.Fatal("MinIO STS fixture returned incomplete credentials")
	}
	return c.Access, c.Secret, c.Token
}
