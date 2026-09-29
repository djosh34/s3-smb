// SPDX-License-Identifier: AGPL-3.0-only
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	smithylog "github.com/aws/smithy-go/logging"
	"github.com/sirupsen/logrus"
)

// Logrus installs a bridge on an actual native logger, retaining native Fatal
// (os.Exit(1)) and Panic behavior. Do not add another stderr/syslog hook to it.
func Logrus(l *logrus.Logger, component string) {
	l.SetOutput(io.Discard)
	l.SetFormatter(nativeFormatter{component: component})
	l.SetLevel(logrus.TraceLevel) // slog owns filtering, including later CLI changes.
}

type nativeFormatter struct{ component string }

func (f nativeFormatter) Format(e *logrus.Entry) ([]byte, error) {
	level := slog.LevelInfo
	switch e.Level {
	case logrus.TraceLevel, logrus.DebugLevel:
		level = slog.LevelDebug
	case logrus.WarnLevel:
		level = slog.LevelWarn
	case logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel:
		level = slog.LevelError
	}
	// logrus panics with this Entry after formatting: never leave the raw message
	// or arbitrary native fields in the panic payload.
	e.Message = Redact(e.Message)
	attrs := []slog.Attr{slog.String("component", f.component)}
	for key, value := range e.Data {
		attrs = append(attrs, cleanAttr(slog.Any(key, value)))
	}
	if e.Level == logrus.PanicLevel {
		e.Data = logrus.Fields{}
	}
	slog.Default().LogAttrs(context.Background(), level, strings.TrimRight(e.Message, "\n"), attrs...)
	return nil, nil
}

// SDKLogger is supplied before AWS SDK construction and region discovery. Never
// enable SDK request/response body, signing, or HTTP-header trace modes: redaction
// is not a substitute for keeping those payloads out of diagnostics.
type SDKLogger struct{}

func (SDKLogger) Logf(classification smithylog.Classification, format string, args ...interface{}) {
	level := slog.LevelDebug
	if classification == smithylog.Warn {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, fmt.Sprintf(format, args...), "component", "aws-sdk")
}
