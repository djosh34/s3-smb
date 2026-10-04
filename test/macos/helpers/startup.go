// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"bytes"
	"errors"
	"regexp"
)

// Startup checks the application's documented PTY consent and readiness messages.
type Startup struct {
	phase     string
	buffer    []byte
	confirmed bool
	reported  bool
}

// NewStartup chooses the only confirmation allowed for this start.
func NewStartup(phase string) *Startup { return &Startup{phase: phase} }

var pointPattern = regexp.MustCompile(`Recover metadata from (\S+)`)

// Observe returns whether to answer yes, whether SMB became ready, and its recovery point.
func (s *Startup) Observe(data []byte) (answer, ready bool, point string, err error) {
	s.buffer = append(s.buffer, data...)
	if len(s.buffer) > 128*1024 {
		s.buffer = s.buffer[len(s.buffer)-128*1024:]
	}
	if bytes.Contains(s.buffer, []byte("Continue? [yes/no]: ")) && !s.confirmed {
		expected := "Initialize a genuinely empty S3 dataset?"
		if s.phase == "recover" {
			expected = "Recover metadata from "
		}
		if s.phase == "restart" || !bytes.Contains(s.buffer, []byte(expected)) {
			return false, false, "", errors.New("unexpected application confirmation; refusing automatic answer")
		}
		s.confirmed, answer = true, true
	}
	if !bytes.Contains(s.buffer, []byte(`"msg":"SMB serving"`)) || s.reported {
		return answer, false, "", nil
	}
	if s.phase != "restart" && !s.confirmed {
		return false, false, "", errors.New("fresh start did not require documented confirmation")
	}
	if match := pointPattern.FindSubmatch(s.buffer); match != nil {
		point = string(match[1])
	}
	if s.phase == "recover" && point == "" {
		return false, false, "", errors.New("recovery did not name a metadata point")
	}
	s.reported = true
	return answer, true, point, nil
}
