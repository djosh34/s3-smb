// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DropLog contains evidence from one cut's bounded macOS log window.
type DropLog struct {
	Refused      bool `json:"refused_non_idempotent"`
	Reconnected  bool `json:"reconnected"`
	BackupStarts int  `json:"backup_starts"`
}

// ParseDropLog recognizes the exact reconnect messages in Apple
// SMBClient-494.120.2/kernel/netsmb/smb_iod.c. Other failures are not refusals.
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
		}
		if strings.HasSuffix(record.Process, "/backupd") && (strings.HasPrefix(record.Message, "Starting manual backup") || strings.HasPrefix(record.Message, "Starting automatic backup")) {
			result.BackupStarts++
		}
	}
	return result, nil
}

// BandWriteReady waits for two advancing Copying samples, at least 128 MiB
// copied and at least 512 MiB left in the harness's four-GiB change.
func BandWriteReady(previous, current string) bool {
	if !Copying(previous) || !Copying(current) {
		return false
	}
	before := copiedBytes(previous)
	after := copiedBytes(current)
	return after > before && after >= 128<<20 && after < (4<<30)-(512<<20)
}

func copiedBytes(text string) float64 {
	match := bytesPattern.FindStringSubmatch(text)
	if match == nil {
		return 0
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0
	}
	return value
}

// DropAttempt records the cut, its completion and the client log classification.
type DropAttempt struct {
	CutAt        time.Time `json:"cut_at"`
	RestoredAt   time.Time `json:"restored_at"`
	StatusAtCut  string    `json:"status_at_cut"`
	CommandError string    `json:"command_error,omitempty"`
	Log          DropLog   `json:"log"`
	Completed    bool      `json:"completed"`
}

// DropReport never calls three refusals a passing reconnect test.
type DropReport struct {
	Status   string        `json:"status"`
	Attempts []DropAttempt `json:"attempts"`
}

// RunDropAttempts retries only the client's explicit non-idempotent refusal.
// Setup, timeout, log and ordinary reconnect errors stop the run at once.
func RunDropAttempts(ctx context.Context, attempt func(int) (DropAttempt, error)) (DropReport, error) {
	report := DropReport{Status: "failed"}
	for number := 1; number <= 3; number++ {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result, err := attempt(number)
		report.Attempts = append(report.Attempts, result)
		if err != nil {
			return report, err
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := CheckShortDrop(result.CutAt, result.RestoredAt); err != nil {
			return report, err
		}
		if result.Log.Refused {
			continue
		}
		if !result.Completed || !result.Log.Reconnected || result.Log.BackupStarts != 1 {
			return report, errors.New("same backup did not complete after reconnect")
		}
		report.Status = "passed"
		return report, nil
	}
	report.Status = "not tested"
	return report, nil
}

// CheckShortDrop prevents a slow runner from counting a long outage as a
// short reconnect test, including attempts where the client refused reconnect.
func CheckShortDrop(cut, restored time.Time) error {
	elapsed := restored.Sub(cut)
	if cut.IsZero() || elapsed <= 0 || elapsed > 30*time.Second {
		return errors.New("short drop was not within the 30-second reconnect window")
	}
	return nil
}

// CheckOutage requires a measured outage beyond the Mac reconnect window,
// a visible failure and no completed backup from the interrupted attempt.
func CheckOutage(cut, restored time.Time, commandErr error, latest, baseline string) error {
	if restored.Sub(cut) <= 30*time.Second {
		return errors.New("outage did not exceed 30 seconds")
	}
	if commandErr == nil {
		return errors.New("long outage did not fail visibly")
	}
	if latest != baseline {
		return errors.New("interrupted backup completed during the long outage")
	}
	return nil
}
