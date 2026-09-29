// SPDX-License-Identifier: AGPL-3.0-only
package xorm

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/logging"
	xlog "github.com/djosh34/s3-smb/internal/thirdparty/xorm/log"
	_ "github.com/mattn/go-sqlite3"
)

func TestLoggingInstalledBeforeConnection(t *testing.T) {
	var out bytes.Buffer
	logging.Install(&out)
	_ = logging.Configure("json", "debug")
	db, err := NewEngine("sqlite3", filepath.Join(t.TempDir(), "missing", "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok := db.Logger().(xlog.SlogLogger); !ok {
		t.Fatalf("unsafe initial logger %T", db.Logger())
	}
	if err := db.Ping(); err == nil {
		t.Fatal("expected real SQL open failure")
	}
	assertFrames(t, out.String())
}
func TestSQLFailureAndExplicitSessionShowSQL(t *testing.T) {
	var out bytes.Buffer
	logging.Install(&out)
	_ = logging.Configure("json", "debug")
	db, err := NewEngine("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.ShowSQL(true)
	session := db.NewSession()
	defer session.Close()
	session.Context(context.WithValue(context.Background(), xlog.SessionShowSQLKey, true))
	if _, err := session.Exec("CREATE TABLE secret_test (v TEXT UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec("INSERT INTO secret_test(v) VALUES (?)", "UNREGISTERED_SQL_SECRET"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec("INSERT INTO secret_test(v) VALUES (?)", "UNREGISTERED_SQL_SECRET"); err == nil {
		t.Fatal("expected unique constraint failure")
	}
	if strings.Contains(out.String(), "UNREGISTERED_SQL_SECRET") || strings.Contains(out.String(), "INSERT INTO") {
		t.Fatalf("payload leak: %s", &out)
	}
	assertFrames(t, out.String())
}
func assertFrames(t *testing.T, text string) {
	t.Helper()
	if text == "" {
		t.Fatal("missing database diagnostics")
	}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
	}
}
