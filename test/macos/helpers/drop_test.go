// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"embed"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/drop/*.json
var samples embed.FS

func TestParseDropLog(t *testing.T) {
	for name, want := range map[string]DropLog{
		"refused":     {Refused: true},
		"reconnected": {Reconnected: true},
		"failed":      {},
	} {
		data, err := samples.ReadFile("testdata/drop/" + name + ".json")
		must(t, err)
		got, err := ParseDropLog(data)
		must(t, err)
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
		// The same message from another sender must not count.
		got, err = ParseDropLog([]byte(strings.ReplaceAll(string(data), "/smbfs\"", "/other\"")))
		must(t, err)
		if got != (DropLog{}) {
			t.Errorf("%s from another sender: %+v", name, got)
		}
	}
	got, err := ParseDropLog([]byte(`[
		{"processImagePath":"/System/Library/CoreServices/TimeMachine/backupd","eventMessage":"Starting backup with mode \"manual backup\""},
		{"processImagePath":"/System/Library/CoreServices/TimeMachine/backupd","eventMessage":"Starting backup with mode \"automatic backup\""},
		{"processImagePath":"/System/Library/CoreServices/TimeMachine/backupd","eventMessage":"Starting query at qos 0x15"},
		{"processImagePath":"/other","eventMessage":"Starting backup with mode \"manual backup\""},
		{"processImagePath":"/other","eventMessage":"Backup failed: BACKUP_FAILED_DISCONNECTED_NETWORK (26)"}
	]`))
	if err != nil || got.BackupStarts != 2 || got.Failed {
		t.Fatal(got, err)
	}
	got, err = ParseDropLog([]byte(`[{"processImagePath":"/System/Library/CoreServices/TimeMachine/backupd","eventMessage":"Backup failed: BACKUP_FAILED_DISCONNECTED_NETWORK (26)"}]`))
	if err != nil || !got.Failed {
		t.Fatal(got, err)
	}
}

func TestRunDropAttempts(t *testing.T) {
	cut := time.Unix(100, 0)
	passed := DropAttempt{CutAt: cut, RestoredAt: cut.Add(5 * time.Second), Completed: true, Log: DropLog{Reconnected: true, BackupStarts: 1}}
	refused := DropAttempt{CutAt: cut, RestoredAt: cut.Add(5 * time.Second), Log: DropLog{Refused: true}}
	newBackup := passed
	newBackup.Log.BackupStarts = 2
	failedBackup := passed
	failedBackup.Log.Failed = true
	slow := refused
	slow.RestoredAt = cut.Add(31 * time.Second)
	for _, test := range []struct {
		status   string
		attempts []DropAttempt
		wantErr  bool
	}{
		{"passed", []DropAttempt{refused, passed}, false},
		{"not tested", []DropAttempt{refused, refused, refused}, false},
		{"failed", []DropAttempt{refused, {CutAt: cut, RestoredAt: cut}}, true},
		{"failed", []DropAttempt{newBackup}, true},
		{"failed", []DropAttempt{failedBackup}, true},
		{"failed", []DropAttempt{slow}, true},
	} {
		report, err := RunDropAttempts(func(number int) DropAttempt { return test.attempts[number-1] })
		if report.Status != test.status || (err != nil) != test.wantErr || len(report.Attempts) != len(test.attempts) {
			t.Error(test.attempts, report, err)
		}
	}
}

func TestCheckOutage(t *testing.T) {
	cut := time.Unix(100, 0)
	failed := DropLog{Failed: true}
	must(t, CheckOutage(cut, cut.Add(45*time.Second), failed, "baseline", "baseline"))
	for _, err := range []error{
		CheckOutage(cut, cut.Add(30*time.Second), failed, "baseline", "baseline"),
		CheckOutage(cut, cut.Add(45*time.Second), DropLog{}, "baseline", "baseline"),
		CheckOutage(cut, cut.Add(45*time.Second), failed, "new", "baseline"),
	} {
		if err == nil {
			t.Error("outage check passed")
		}
	}
}
