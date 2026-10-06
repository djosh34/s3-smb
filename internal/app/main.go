// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/logging"
)

// Main runs the foreground process and returns its exit code. When shutdown
// exceeds its deadline, Main calls exit while the folder lock is still held,
// because storage I/O may still be running.
func Main(args []string, version string, stdout, stderr io.Writer, exit func(code int)) int {
	logging.Install(stderr)
	// Apply --log-format before parsing, so an error in the command line or the
	// config is reported in that format too.
	if format := logOverride(args); format != "" {
		if err := logging.Configure(format, "info"); err != nil {
			slog.Error("command line error", "error", err)
			return 2
		}
	}
	a, err := parseArguments(args)
	if err != nil {
		slog.Error("command line error", "error", err)
		return 2
	}
	switch a.command {
	case "help":
		return write(stdout, usage)
	case "version":
		return write(stdout, "s3-smb "+version+"\nSource: https://github.com/djosh34/s3-smb\n")
	}
	hardExit := func() {
		logFailure("hard shutdown deadline exceeded; exiting with the folder lock retained until process termination", nil)
		exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A signal can arrive during a blocked startup step, where serve cannot
	// clean up. Exit once the shutdown deadline has passed.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
		case <-finished:
			return
		}
		timer := time.NewTimer(shutdownTimeout)
		defer timer.Stop()
		select {
		case <-finished:
			return
		case <-timer.C:
			hardExit()
		}
	}()
	if a.configPath == "" {
		a.configPath, err = config.DefaultPath()
		if err != nil {
			slog.Error("configuration error", "error", err)
			return 1
		}
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 1
	}
	format := cfg.Logging.Format
	if a.logFormat != "" {
		format = a.logFormat
	}
	if err = logging.Configure(format, cfg.Logging.Level); err != nil {
		slog.Error("logging configuration error", "error", err)
		return 1
	}
	resolved, err := cfg.Resolve(ctx, slog.Default())
	if err != nil {
		slog.Error("resolve startup credentials or TLS failed", "error", err)
		return 1
	}
	if err = serve(ctx, resolved, hardExit); err != nil {
		logFailure("service stopped with failure", err)
		return 1
	}
	return 0
}

const shutdownTimeout = 30 * time.Second

func write(w io.Writer, text string) int {
	if _, err := io.WriteString(w, text); err != nil {
		return 1
	}
	return 0
}

func logFailure(message string, err error) {
	// A blocked log pipe would keep the process from exiting. Wait 100 ms for the
	// message, then return so the caller can exit.
	done := make(chan struct{})
	go func() {
		if err != nil {
			slog.Error(message, "error", err)
		} else {
			slog.Error(message)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}
}
