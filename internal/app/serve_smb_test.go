// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/storage"
)

type servingHandler struct {
	slog.Handler
	ready chan string
}

func (h servingHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "SMB serving" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "address" {
				h.ready <- attr.Value.String()
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, record)
}

func TestSMBServeCancellation(t *testing.T) {
	stateDir := t.TempDir()
	format, err := storage.NewFormat(storage.VolumeName, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := storage.OpenMetadata(filepath.Join(stateDir, "metadata.db"), meta.DefaultConf())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Init(format, false); err != nil {
		t.Fatal(err)
	}
	if err = m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// format.json holds no bucket or credentials.
	identity, err := json.Marshal(struct {
		meta.Format
		Bucket       string `json:"-"`
		AccessKey    string `json:"-"`
		SecretKey    string `json:"-"`
		SessionToken string `json:"-"`
	}{Format: *format})
	if err != nil {
		t.Fatal(err)
	}
	// Only startup reads are needed for an existing, read-only dataset.
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []byte
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			data = []byte(`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>s3-smb/format.json</Key><Size>1</Size></Contents></ListBucketResult>`)
		case r.Method == http.MethodGet && r.URL.Path == "/fixture/s3-smb/format.json":
			data = identity
		case r.Method == http.MethodGet && r.URL.Path == "/fixture/s3-smb/juicefs_uuid":
			data = []byte(format.UUID)
		default:
			t.Errorf("unexpected S3 request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
		if _, e := w.Write(data); e != nil {
			t.Error(e)
		}
	}))
	defer s3.Close()
	ready := make(chan string, 1)
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(servingHandler{slog.NewTextHandler(io.Discard, nil), ready}))
	defer slog.SetDefault(previousLogger)
	zero := config.ByteSize(0)
	pathStyle := true
	c := &config.Resolved{
		Config: &config.Config{
			SMB:     serverConfig(),
			Storage: config.StorageConfig{StateDir: stateDir, CacheDir: t.TempDir(), CacheSize: &zero},
			S3:      config.S3Config{Bucket: "fixture", Region: "us-east-1", Endpoint: s3.URL, PathStyle: &pathStyle},
			Backup:  config.BackupConfig{Interval: time.Hour},
		},
		AccessKey: "synthetic-access", SecretKey: "synthetic-secret",
	}
	c.SMB.ReadOnly = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, c, func() { t.Error("hard exit") })
		close(done)
	}()
	var address string
	select {
	case address = <-ready:
	case err = <-done:
		t.Fatalf("serve stopped before listening: %v", err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatalf("canceled serve: %v", err)
	}
	if conn, e := new(net.Dialer).DialContext(t.Context(), "tcp", address); e == nil {
		t.Fatal(errors.Join(errors.New("serve left its listener open"), conn.Close()))
	}
	lock, err := lockState(stateDir)
	if err != nil {
		t.Fatalf("serve retained its state lock: %v", err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
}
