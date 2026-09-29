// SPDX-License-Identifier: AGPL-3.0-only
package log

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/logging"
)

func TestSlogNeverEmitsSQLArguments(t *testing.T) {
	var out bytes.Buffer
	logging.Install(&out)
	if err := logging.Configure("json", "debug"); err != nil {
		t.Fatal(err)
	}
	l := SlogLogger{}
	l.ShowSQL(true)
	if l.IsShowSQL() {
		t.Fatal("SQL payload logging enabled")
	}
	l.Debugf("[cache] Get SQL: %s, %v", "SQL_LITERAL_SECRET", []any{"SQL_ARGUMENT_SECRET"})
	l.Warnf("[cache] bean %v", struct{ Password string }{"SQL_BEAN_SECRET"})
	l.Errorf("byte2Time error: %v", "SQL_CONVERSION_SECRET")
	// Explicit session ShowSQL(true) can call AfterSQL even with IsShowSQL false.
	l.AfterSQL(LogContext{SQL: "SELECT SQL_LITERAL_SECRET", Args: []any{"SQL_ARGUMENT_SECRET"}, ExecuteTime: time.Millisecond})
	for _, marker := range []string{"SQL_LITERAL_SECRET", "SQL_ARGUMENT_SECRET", "SQL_BEAN_SECRET", "SQL_CONVERSION_SECRET"} {
		if strings.Contains(out.String(), marker) {
			t.Fatalf("leaked %s", marker)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want diagnostics, got %s", &out)
	}
	for _, line := range lines {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
	}
}
