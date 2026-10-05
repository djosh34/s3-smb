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

// Logrus routes a logrus logger through slog. Fatal still exits with status 1
// and Panic still panics.
func Logrus(l *logrus.Logger, component string) {
	l.SetOutput(io.Discard)
	l.SetFormatter(logrusFormatter{component: component})
	l.SetLevel(logrus.TraceLevel) // slog filters by level
}

type logrusFormatter struct{ component string }

func (f logrusFormatter) Format(e *logrus.Entry) ([]byte, error) {
	level := slog.LevelInfo
	switch e.Level {
	case logrus.TraceLevel, logrus.DebugLevel:
		level = slog.LevelDebug
	case logrus.InfoLevel:
	case logrus.WarnLevel:
		level = slog.LevelWarn
	case logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel:
		level = slog.LevelError
	}
	// logrus panics with this entry after formatting it. Redact the message and,
	// below, drop the fields, so the panic output holds no secret.
	e.Message = Redact(e.Message)
	attrs := []slog.Attr{slog.String("component", f.component), slog.String("logrus_level", e.Level.String())}
	for key, value := range e.Data {
		attrs = append(attrs, cleanAttr(slog.Any(key, value)))
	}
	if e.Level == logrus.PanicLevel {
		e.Data = logrus.Fields{}
	}
	slog.Default().LogAttrs(context.Background(), level, strings.TrimRight(e.Message, "\n"), attrs...)
	return nil, nil
}

// SDKLogger forwards AWS SDK warnings and debug lines to slog. The SDK's
// request, signing and body traces stay off.
type SDKLogger struct{}

// Logf logs one SDK line.
func (SDKLogger) Logf(classification smithylog.Classification, format string, args ...interface{}) {
	level := slog.LevelDebug
	if classification == smithylog.Warn {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, fmt.Sprintf(format, args...), "component", "aws-sdk")
}
