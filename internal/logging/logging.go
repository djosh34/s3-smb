// SPDX-License-Identifier: AGPL-3.0-only
// Package logging writes every log line of the daemon through one slog handler
// that redacts secrets. Call Install before parsing configuration and Configure
// after it.
package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

var state = struct {
	sync.RWMutex
	writer  io.Writer
	handler slog.Handler
	secrets []string
}{writer: os.Stderr}

// Install sets text output at INFO and routes the standard log package and the
// standard logrus logger through it. Registered secrets stay registered.
func Install(w io.Writer) {
	if w == nil {
		w = os.Stderr
	}
	state.Lock()
	state.writer = w
	state.handler = slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	state.Unlock()
	slog.SetDefault(slog.New(&handler{}))
	log.SetFlags(0)
	log.SetPrefix("")
	Logrus(logrus.StandardLogger(), "logrus")
}

// Configure checks both settings before it changes the logger. Empty values
// select text and info. The error omits an invalid value, which may be a secret.
func Configure(format, level string) error {
	if format == "" {
		format = "text"
	}
	var l slog.Level
	switch strings.ToLower(level) {
	case "", "info":
		l = slog.LevelInfo
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return fmt.Errorf("invalid log level (use debug, info, warn or error)")
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("invalid log format (use text or json)")
	}
	state.Lock()
	defer state.Unlock()
	opts := &slog.HandlerOptions{Level: l}
	if format == "json" {
		state.handler = slog.NewJSONHandler(state.writer, opts)
	} else {
		state.handler = slog.NewTextHandler(state.writer, opts)
	}
	return nil
}

// RegisterSecret adds values to redact from every log line. It skips empty
// values. Redaction replaces longer secrets first.
func RegisterSecret(values ...string) {
	state.Lock()
	defer state.Unlock()
	for _, value := range values {
		if value == "" {
			continue
		}
		quoted := strconv.Quote(value)
		jsonQuoted, _ := json.Marshal(value)
		// A secret can reach the log quoted by %q, as JSON or URL-escaped.
		// Redact those forms too.
		for _, v := range []string{value, url.QueryEscape(value), url.PathEscape(value), quoted[1 : len(quoted)-1], string(jsonQuoted[1 : len(jsonQuoted)-1])} {
			found := false
			for _, old := range state.secrets {
				if old == v {
					found = true
					break
				}
			}
			if !found {
				state.secrets = append(state.secrets, v)
			}
		}
	}
	sort.Slice(state.secrets, func(i, j int) bool { return len(state.secrets[i]) > len(state.secrets[j]) })
}

// Redact replaces registered secrets in value. The logrus bridge also uses it
// on panic messages, which the Go runtime prints without slog.
func Redact(value string) string {
	state.RLock()
	defer state.RUnlock()
	for _, secret := range state.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

// bound keeps a logger's attributes and groups until a record is written, so
// secrets registered later and a changed output format apply to it too.
type bound struct {
	groups []string
	attrs  []slog.Attr
}
type handler struct {
	groups []string
	bound  []bound
}

func (*handler) Enabled(ctx context.Context, level slog.Level) bool {
	state.RLock()
	h := state.handler
	state.RUnlock()
	return h != nil && h.Enabled(ctx, level)
}
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.bound = append(append([]bound(nil), h.bound...), bound{append([]string(nil), h.groups...), append([]slog.Attr(nil), attrs...)})
	return &n
}
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.groups = append(append([]string(nil), h.groups...), name)
	return &n
}
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	state.RLock()
	out := state.handler
	state.RUnlock()
	if out == nil {
		return nil
	}
	record := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	applied := 0
	for _, b := range h.bound {
		for _, name := range b.groups[applied:] {
			out = out.WithGroup(Redact(name))
		}
		applied = len(b.groups)
		out = out.WithAttrs(cleanAttrs(b.attrs))
	}
	for _, name := range h.groups[applied:] {
		out = out.WithGroup(Redact(name))
	}
	r.Attrs(func(a slog.Attr) bool { record.AddAttrs(cleanAttr(a)); return true })
	return out.Handle(ctx, record)
}
func cleanAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = cleanAttr(a)
	}
	return out
}
func cleanAttr(a slog.Attr) slog.Attr {
	key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(a.Key, "_", ""), "-", ""))
	switch key {
	case "password", "passphrase", "secret", "secretkey", "accesskey", "sessiontoken", "token", "authorization", "privatekey", "keypem", "config", "configuration", "argv", "args", "sqlargs", "xattrvalue", "helperoutput":
		return slog.String(Redact(a.Key), "[REDACTED]")
	}
	a.Key = Redact(a.Key)
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindGroup:
		a.Value = slog.GroupValue(cleanAttrs(a.Value.Group())...)
	case slog.KindString:
		a.Value = slog.StringValue(Redact(a.Value.String()))
	case slog.KindAny:
		a.Value = slog.StringValue(Redact(fmt.Sprint(a.Value.Any())))
	}
	return a
}
