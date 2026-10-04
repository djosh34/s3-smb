// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import "testing"

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
