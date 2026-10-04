//go:build smbnext

// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"syscall"
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

func TestSMBNextServeCancellation(t *testing.T) {
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
	// SID-zero rows from the old adapter are unrelated to the new server.
	if eno := m.Setlk(meta.Background(), meta.RootInode, 1, false, syscall.F_WRLCK, 0, 11, 1); eno != 0 {
		t.Fatal(eno)
	}
	if eno := m.Flock(meta.Background(), meta.RootInode, 1, syscall.F_WRLCK, false); eno != 0 {
		t.Fatal(eno)
	}
	if err = m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	identity, err := json.Marshal(format)
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
		if _, err := w.Write(data); err != nil {
			t.Error(err)
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
		done <- serve(ctx, c)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	var address string
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("serve stopped before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after cancellation")
	}
	if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("serve left its listener open")
	}
	conf := meta.DefaultConf()
	conf.ReadOnly = true
	m, err = storage.OpenMetadata(filepath.Join(stateDir, "metadata.db"), conf)
	if err != nil {
		t.Fatal(err)
	}
	plocks, flocks, listErr := m.ListLocks(t.Context(), meta.RootInode)
	if closeErr := m.Shutdown(); listErr != nil || closeErr != nil {
		t.Fatalf("inspect native locks: %v, %v", listErr, closeErr)
	}
	if len(plocks) != 1 || len(flocks) != 1 {
		t.Fatalf("new server changed native locks: plocks=%v, flocks=%v", plocks, flocks)
	}
	lock, err := lockState(stateDir)
	if err != nil {
		t.Fatalf("serve retained its state lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
