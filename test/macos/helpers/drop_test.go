// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/drop/*.json
var dropSamples embed.FS

func dropSample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := dropSamples.ReadFile("testdata/drop/" + name + ".json")
	must(t, err)
	return data
}

func TestParseDropLog(t *testing.T) {
	for _, test := range []struct {
		name string
		want DropLog
	}{
		{"refused", DropLog{Refused: true}},
		{"reconnected", DropLog{Reconnected: true}},
		{"failed", DropLog{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseDropLog(dropSample(t, test.name))
			must(t, err)
			if got != test.want {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
	for _, data := range [][]byte{nil, []byte("not json"), []byte(`[{"eventMessage":42}]`)} {
		if _, err := ParseDropLog(data); err == nil {
			t.Fatal("invalid log accepted", string(data))
		}
	}
	// A mention from another process must never excuse a reconnect failure.
	data := strings.ReplaceAll(string(dropSample(t, "refused")), "/smbfs\"", "/unrelated\"")
	got, err := ParseDropLog([]byte(data))
	must(t, err)
	if got.Refused {
		t.Fatal("unrelated log counted as refusal")
	}
	for _, message := range []string{"Unknown non idempotent command 0x5", "Reconnect failed", "CREATE in flight", "LOCK failed", "SET_INFO failed"} {
		logData, logErr := json.Marshal([]map[string]string{{"eventMessage": message, "senderImagePath": "/smbfs"}})
		must(t, logErr)
		classified, logErr := ParseDropLog(logData)
		must(t, logErr)
		if classified.Refused {
			t.Fatal("generic failure counted as refusal", message)
		}
	}
	got, err = ParseDropLog([]byte(`[
		{"processImagePath":"/System/Library/CoreServices/TimeMachine/backupd","eventMessage":"Starting manual backup"},
		{"processImagePath":"/System/Library/CoreServices/TimeMachine/backupd","eventMessage":"Starting automatic backup"},
		{"processImagePath":"/other","eventMessage":"Starting manual backup"}
	]`))
	must(t, err)
	if got.BackupStarts != 2 {
		t.Fatal(got)
	}
}

func copyingSample(bytes string) string {
	return "Running = 1;\nBackupPhase = Copying;\nbytes = " + bytes + ";"
}

func TestBandWriteReady(t *testing.T) {
	for _, test := range []struct {
		name, before, after string
		want                bool
	}{
		{"advancing", "134217728", "268435456", true},
		{"threshold", "1", "134217728", true},
		{"scientific", "1e8", "2e8", true},
		{"quoted", "1", `"134217728"`, true},
		{"too early", "1", "134217727", false},
		{"stalled", "134217728", "134217728", false},
		{"regressed", "268435456", "134217728", false},
		{"too late", "268435456", "3758096384", false},
		{"negative", "1", "-1", false},
		{"overflow", "1", "1e999", false},
		{"not a number", "1", "NaN", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := BandWriteReady(copyingSample(test.before), copyingSample(test.after)); got != test.want {
				t.Fatal(got, test.want)
			}
		})
	}
	for _, bad := range []string{"", "Running = 0;", strings.ReplaceAll(copyingSample("268435456"), "Copying", "Finishing")} {
		if BandWriteReady(bad, copyingSample("268435456")) || BandWriteReady(copyingSample("134217728"), bad) {
			t.Fatal("cut outside active Copying", bad)
		}
	}
}

func TestRunDropAttempts(t *testing.T) {
	passed := DropAttempt{Completed: true, Log: DropLog{Reconnected: true, BackupStarts: 1}}
	refused := DropAttempt{Log: DropLog{Refused: true}}
	for _, test := range []struct {
		name     string
		status   string
		attempts []DropAttempt
		wantErr  bool
	}{
		{"first", "passed", []DropAttempt{passed}, false},
		{"second", "passed", []DropAttempt{refused, passed}, false},
		{"third", "passed", []DropAttempt{refused, refused, passed}, false},
		{"three refusals", "not tested", []DropAttempt{refused, refused, refused}, false},
		{"ordinary failure", "failed", []DropAttempt{{}}, true},
		{"failure after refusal", "failed", []DropAttempt{refused, {}}, true},
		{"no reconnect evidence", "failed", []DropAttempt{{Completed: true}}, true},
		{"command failure", "failed", []DropAttempt{{Log: passed.Log}}, true},
		{"new backup", "failed", []DropAttempt{{Completed: true, Log: DropLog{Reconnected: true, BackupStarts: 2}}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			report, err := RunDropAttempts(t.Context(), func(number int) (DropAttempt, error) {
				calls++
				if number != calls || calls > len(test.attempts) {
					t.Fatal("wrong retry count", number, calls)
				}
				return test.attempts[calls-1], nil
			})
			if report.Status != test.status || (err != nil) != test.wantErr || calls != len(test.attempts) || len(report.Attempts) != calls {
				t.Fatal(report, err, calls)
			}
		})
	}
	boom := errors.New("log collection failed")
	report, err := RunDropAttempts(t.Context(), func(int) (DropAttempt, error) { return refused, boom })
	if !errors.Is(err, boom) || report.Status != "failed" || len(report.Attempts) != 1 {
		t.Fatal(report, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	report, err = RunDropAttempts(ctx, func(int) (DropAttempt, error) { calls++; cancel(); return refused, nil })
	if !errors.Is(err, context.Canceled) || calls != 1 || report.Status != "failed" {
		t.Fatal(report, err, calls)
	}
}

func TestCheckOutage(t *testing.T) {
	cut := time.Unix(100, 0)
	failure := errors.New("tmutil exited 1")
	for _, test := range []struct {
		err      error
		latest   string
		duration time.Duration
		wantErr  bool
	}{
		{failure, "baseline", 45 * time.Second, false},
		{failure, "baseline", 30*time.Second + time.Nanosecond, false},
		{failure, "baseline", 30 * time.Second, true},
		{failure, "baseline", 5 * time.Second, true},
		{failure, "baseline", -time.Second, true},
		{nil, "baseline", 45 * time.Second, true},
		{failure, "new-backup", 45 * time.Second, true},
	} {
		t.Run(fmt.Sprintf("%s-%v-%s", test.duration, test.err, test.latest), func(t *testing.T) {
			if err := CheckOutage(cut, cut.Add(test.duration), test.err, test.latest, "baseline"); (err != nil) != test.wantErr {
				t.Fatal(err)
			}
		})
	}
}

func FuzzParseDropLog(f *testing.F) {
	for _, name := range []string{"refused", "reconnected", "failed"} {
		data, err := dropSamples.ReadFile("testdata/drop/" + name + ".json")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"eventMessage":42}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		result, err := ParseDropLog(data)
		if err != nil && result != (DropLog{}) {
			t.Fatal("partial classification on malformed log", result)
		}
	})
}
