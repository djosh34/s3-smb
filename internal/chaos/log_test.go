// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/djosh34/s3-smb/internal/logging"
)

func TestCheckDaemonLogBridge(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	for _, format := range []string{"json", "text"} {
		for _, level := range []logrus.Level{logrus.FatalLevel, logrus.PanicLevel, logrus.ErrorLevel} {
			t.Run(format+"/"+level.String(), func(t *testing.T) {
				var output bytes.Buffer
				var handler slog.Handler
				if format == "json" {
					handler = slog.NewJSONHandler(&output, nil)
				} else {
					handler = slog.NewTextHandler(&output, nil)
				}
				slog.SetDefault(slog.New(handler))
				logger := logrus.New()
				logging.Logrus(logger, "juicefs")
				// Invoke the real bridge without logrus's fatal exit or panic.
				_, err := logger.Formatter.Format(&logrus.Entry{Logger: logger, Level: level, Message: "upload failed", Data: logrus.Fields{}})
				if err != nil {
					t.Fatal(err)
				}
				err = CheckDaemonLog(output.Bytes())
				wantFailure := level == logrus.FatalLevel || level == logrus.PanicLevel
				if (err != nil) != wantFailure {
					t.Fatalf("failure=%t, want %t, log=%s", err != nil, wantFailure, output.Bytes())
				}
			})
		}
	}
}

func TestCheckDaemonLog(t *testing.T) {
	for _, log := range []string{
		"panic: broken\ngoroutine 1", "fatal error: concurrent map writes",
		"WARNING: DATA RACE", `{"level":"fatal","message":"broken"}`,
		`{"level": "panic", "message":"broken"}`, "2026-10-04 FATAL broken",
	} {
		if err := CheckDaemonLog([]byte("ordinary line\n" + log)); err == nil {
			t.Fatalf("missed failure: %s", log)
		}
	}
	for _, log := range []string{"", `{"level":"error","message":"S3 request failed"}`, "panic-free shutdown", "panic_count=0", "not a data race report"} {
		if err := CheckDaemonLog([]byte(log)); err != nil {
			t.Fatal(err)
		}
	}
}
