package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestReceiveErrorLogsConnectionClosed(t *testing.T) {
	options := testOptions(t)
	logs := &recordedHandler{}
	options.Logger = slog.New(logs)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	if err := client.SendRaw(ctx, []byte{1, 0, 0, 32}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err == nil {
		t.Fatal("invalid frame received a reply")
	}
	entries := logs.snapshot()
	if len(entries) != 1 || entries[0].message != "connection closed" {
		t.Fatalf("receive error wording: %+v", entries)
	}
}

func TestRequestLogLevelsDistinguishCancellation(t *testing.T) {
	for _, test := range []struct {
		err    error
		name   string
		level  slog.Level
		status smb.Status
	}{
		{name: "cancelled", err: fmt.Errorf("read stopped: %w", context.Canceled), level: slog.LevelDebug, status: smb.StatusCancelled},
		{name: "backend error", err: errors.New("backend failed"), level: slog.LevelError, status: smb.StatusInternalError},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, release := controlledAsync(t, wire.Read, reply{}, test.err)
			logs := &recordedHandler{}
			server.options.Logger = slog.New(logs)
			client, ctx := corePipeClient(t, server)
			exchange(ctx, t, client, negotiateMessage(t, 1))
			exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))
			close(release)
			final, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if final.Messages[0].Header.Status != test.status {
				t.Fatalf("request status: %+v", final.Messages[0].Header)
			}
			entries := logs.snapshot()
			if len(entries) != 1 || entries[0].level != test.level {
				t.Fatalf("request log level: %+v", entries)
			}
		})
	}
}
