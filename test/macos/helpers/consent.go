// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"errors"
	"strings"
)

// Confirmation accepts only the expected start prompt for phase, initialize or
// recover, and returns the metadata point a recovery names.
func Confirmation(phase, text string) (string, error) {
	expected := "Initialize a genuinely empty S3 dataset?"
	if phase == "recover" {
		expected = "Recover metadata from "
	}
	if (phase != "initialize" && phase != "recover") || !strings.Contains(text, expected) || !strings.Contains(text, "Continue? [yes/no]: ") {
		return "", errors.New("unexpected application confirmation; refusing automatic answer")
	}
	if phase == "initialize" {
		return "", nil
	}
	_, tail, _ := strings.Cut(text, expected)
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return "", errors.New("recovery did not name a metadata point")
	}
	return fields[0], nil
}
