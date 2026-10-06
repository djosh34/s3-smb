// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
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

// serveConfig is a configuration for serve on an in-memory S3 server.
func serveConfig(t *testing.T) *config.Resolved {
	t.Helper()
	pathStyle := true
	return &config.Resolved{
		Config: &config.Config{
			SMB:     serverConfig(),
			Storage: config.StorageConfig{StateDir: t.TempDir()},
			S3:      config.S3Config{Bucket: "bucket", Endpoint: smbtest.NewS3(t), PathStyle: &pathStyle},
		},
		AccessKey: "synthetic-access", SecretKey: "synthetic-secret",
	}
}

func TestSMBServeCancellation(t *testing.T) {
	c := serveConfig(t)
	stateDir := c.Storage.StateDir
	ready := make(chan string, 1)
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(servingHandler{slog.NewTextHandler(io.Discard, nil), ready}))
	defer slog.SetDefault(previousLogger)
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
	case err := <-done:
		t.Fatalf("serve stopped before listening: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("canceled serve: %v", err)
	}
	if conn, e := new(net.Dialer).DialContext(t.Context(), "tcp", address); e == nil {
		t.Fatal(errors.Join(errors.New("serve left its listener open"), conn.Close()))
	}
	lock, err := lockState(stateDir)
	if err != nil {
		t.Fatalf("serve retained its folder lock: %v", err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// The configured capacity and read-only mode reach the engine.
func TestOpenPassesStorageSettings(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		c := serveConfig(t)
		c.SMB.ReadOnly = readOnly
		c.Storage.Capacity = 2 << 40
		var r resources
		err := r.open(t.Context(), c)
		t.Cleanup(func() {
			if e := r.close(t.Context()); e != nil {
				t.Error(e)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		space, err := r.store.StatFS(t.Context())
		if err != nil || space.Capacity != 2<<40 {
			t.Fatalf("capacity: %+v, %v", space, err)
		}
		resolved, err := r.store.Lookup(t.Context(), "fixture")
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.store.Create(t.Context(), resolved.Name, smb.KindFile)
		if readOnly && !errors.Is(err, smb.ErrReadOnly) || !readOnly && err != nil {
			t.Fatalf("read_only=%t: create error %v", readOnly, err)
		}
	}
}
