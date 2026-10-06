// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DropLog is what the macOS log shows around one connection cut.
type DropLog struct {
	BackupStarts int  `json:"backup_starts"`
	Refused      bool `json:"refused_non_idempotent"`
	Reconnected  bool `json:"reconnected"`
	// Failed is backupd's "Backup failed" line, the failure a user sees.
	// tmutil startbackup --block exits 0 also when the backup fails.
	Failed bool `json:"backup_failed"`
	// TimedOut counts requests that smbfs failed after 2 minutes without a
	// reply. Data written through such a request can be lost silently.
	TimedOut int `json:"timed_out_requests"`
}

// ParseDropLog reads `log show --style json` output. It matches the exact
// reconnect messages in Apple's SMBClient-494.120.2 kernel/netsmb/smb_iod.c, so
// other reconnect failures do not count as refusals.
func ParseDropLog(data []byte) (DropLog, error) {
	var records []struct {
		Message string `json:"eventMessage"`
		Sender  string `json:"senderImagePath"`
		Process string `json:"processImagePath"`
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return DropLog{}, fmt.Errorf("decode macOS drop log: %w", err)
	}
	var result DropLog
	for _, record := range records {
		if strings.HasSuffix(record.Sender, "/smbfs") {
			result.Refused = result.Refused || strings.Contains(record.Message, "Non idempotent requests found, failing reconnect")
			result.Reconnected = result.Reconnected || strings.Contains(record.Message, "Reconnect completed successfully.")
			if strings.Contains(record.Message, "Timed out waiting on the response") {
				result.TimedOut++
			}
		}
		// macOS 15.7 logs `Starting backup with mode "manual backup"`.
		if strings.HasSuffix(record.Process, "/backupd") && strings.HasPrefix(record.Message, "Starting backup with mode ") {
			result.BackupStarts++
		}
		// The window opens before the attempt, so an earlier backup's end can
		// be in it. Only a failure after this attempt's start counts.
		if result.BackupStarts > 0 && strings.HasSuffix(record.Process, "/backupd") && strings.HasPrefix(record.Message, "Backup failed") {
			result.Failed = true
		}
	}
	return result, nil
}

// BandWriteReady reports whether two tmutil status samples show Copying
// advancing, with at least 128 MiB copied and at least 512 MiB of the four-GiB
// change left.
func BandWriteReady(previous, current string) bool {
	if !Copying(previous) || !Copying(current) {
		return false
	}
	before := copiedBytes(previous)
	after := copiedBytes(current)
	return after > before && after >= 128<<20 && after < (4<<30)-(512<<20)
}

// DropAttempt records one cut, whether the backup completed and its log.
type DropAttempt struct {
	CutAt        time.Time `json:"cut_at"`
	RestoredAt   time.Time `json:"restored_at"`
	StatusAtCut  string    `json:"status_at_cut"`
	CommandError string    `json:"command_error,omitempty"`
	Log          DropLog   `json:"log"`
	Completed    bool      `json:"completed"`
}

// DropReport is the outcome of the short drop test: passed, failed or not
// tested.
type DropReport struct {
	Status   string        `json:"status"`
	Attempts []DropAttempt `json:"attempts"`
}

// RunDropAttempts runs up to three attempts. Only the client's explicit
// non-idempotent refusal leads to another attempt. Three refusals are "not
// tested", which the harness reports as a failure.
func RunDropAttempts(attempt func(int) DropAttempt) (DropReport, error) {
	report := DropReport{Status: "failed"}
	for number := 1; number <= 3; number++ {
		result := attempt(number)
		report.Attempts = append(report.Attempts, result)
		// A slow runner must not turn a long outage into a short drop attempt.
		if result.RestoredAt.Sub(result.CutAt) > 30*time.Second {
			return report, errors.New("short drop was not within the 30-second reconnect window")
		}
		if result.Log.Refused {
			continue
		}
		if !result.Completed || !result.Log.Reconnected || result.Log.BackupStarts != 1 || result.Log.Failed {
			return report, errors.New("same backup did not complete after reconnect")
		}
		report.Status = "passed"
		return report, nil
	}
	report.Status = "not tested"
	return report, nil
}

// CheckOutage requires an outage longer than the Mac reconnect window, a
// visible backup failure and no completed backup from the interrupted attempt.
func CheckOutage(cut, restored time.Time, log DropLog, latest, baseline string) error {
	if restored.Sub(cut) <= 30*time.Second {
		return errors.New("outage did not exceed 30 seconds")
	}
	if !log.Failed {
		return errors.New("long outage did not fail visibly")
	}
	if latest != baseline {
		return errors.New("interrupted backup completed during the long outage")
	}
	return nil
}
