// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"strconv"
	"strings"
)

// SMBLogging keeps the original kernel log level for cleanup and readback evidence.
type SMBLogging struct {
	Previous string `json:"previous"`
	Active   string `json:"active"`
}

// EnableSMBLogging enables SMBWARNING messages after the first mount loads
// smbfs. The returned state must be restored even when writing or readback fails.
func EnableSMBLogging(command func(...string) (string, error)) (SMBLogging, error) {
	previous, err := smbLogLevel(command)
	if err != nil {
		return SMBLogging{}, err
	}
	state := SMBLogging{Previous: previous}
	if _, err = command("-w", "net.smb.fs.loglevel=1"); err != nil {
		return state, err
	}
	state.Active, err = smbLogLevel(command)
	if err != nil {
		return state, err
	}
	if state.Active == "0" {
		return state, errors.New("SMB warning logging is still disabled")
	}
	return state, nil
}

func smbLogLevel(command func(...string) (string, error)) (string, error) {
	output, err := command("-n", "net.smb.fs.loglevel")
	if err != nil {
		return "", err
	}
	level := strings.TrimSpace(output)
	if _, err := strconv.ParseUint(level, 10, 32); err != nil {
		return "", err
	}
	return level, nil
}

// Restore resets the saved kernel log level. No saved state means no change.
func (logging SMBLogging) Restore(command func(...string) (string, error)) error {
	if logging.Previous == "" {
		return nil
	}
	_, err := command("-w", "net.smb.fs.loglevel="+logging.Previous)
	return err
}
