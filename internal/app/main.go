// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/logging"
)

// Main is the foreground process entry point. A hard deadline terminates the
// process rather than releasing a lock while native I/O may still be running.
func Main(args []string, version string) int {
	logging.Install(os.Stderr)
	if override := logOverride(args); override != "" {
		_ = logging.Configure(override, "info")
	}
	a, err := parseArguments(args)
	if err != nil {
		slog.Error("command line error", "error", err)
		return 2
	}
	switch a.command {
	case "help":
		printHelp(os.Stdout)
		return 0
	case "version":
		fmt.Fprintln(os.Stdout, "s3-smb "+version)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// This also bounds a signal received during a blocked startup operation or tty
	// read, before the ordinary resource cleanup path can run.
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
	if cfg.SMB.Password != nil {
		logging.RegisterSecret(*cfg.SMB.Password)
	}
	resolved, err := cfg.Resolve(ctx, slog.Default())
	if err != nil {
		slog.Error("resolve startup credentials or TLS failed", "error", err)
		return 1
	}
	logging.RegisterSecret(resolved.AccessKey, resolved.SecretKey, resolved.SessionToken, resolved.Passphrase)
	if err = serve(ctx, resolved); err != nil {
		slog.Error("service stopped with failure", "error", err)
		return 1
	}
	return 0
}

const shutdownTimeout = 30 * time.Second

func hardExit() {
	slog.Error("hard shutdown deadline exceeded; exiting with local state lock retained until process termination")
	os.Exit(1)
}
