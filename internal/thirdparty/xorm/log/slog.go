// SPDX-License-Identifier: AGPL-3.0-only
package log

import (
	"context"
	"log/slog"
)

// SlogLogger is installed by the bundled engine before any Ping or schema work.
// Native diagnostics sometimes include complete beans, SQL and parameters, even
// outside ShowSQL. Keep the diagnostic template but never interpolate arguments.
// The caller receives the actual database error; the app logs its safe context.
type SlogLogger struct{}

var _ ContextLogger = SlogLogger{}

func (SlogLogger) Debugf(format string, _ ...interface{}) { diagnostic(slog.LevelDebug, format) }
func (SlogLogger) Infof(format string, _ ...interface{})  { diagnostic(slog.LevelInfo, format) }
func (SlogLogger) Warnf(format string, _ ...interface{})  { diagnostic(slog.LevelWarn, format) }
func (SlogLogger) Errorf(format string, _ ...interface{}) { diagnostic(slog.LevelError, format) }
func diagnostic(level slog.Level, template string) {
	slog.Log(context.Background(), level, "database diagnostic", "component", "xorm", "operation", template)
}
func (SlogLogger) Level() LogLevel      { return LOG_DEBUG }
func (SlogLogger) SetLevel(LogLevel)    {} // the process slog level is authoritative
func (SlogLogger) ShowSQL(...bool)      {}
func (SlogLogger) IsShowSQL() bool      { return false }
func (SlogLogger) BeforeSQL(LogContext) {}
func (SlogLogger) AfterSQL(ctx LogContext) {
	// Also safe if a session explicitly enables ShowSQL, bypassing IsShowSQL.
	slog.Debug("database operation completed", "component", "xorm", "duration", ctx.ExecuteTime)
}
