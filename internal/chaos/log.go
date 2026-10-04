// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"bytes"
	"fmt"
	"regexp"
)

var daemonFailure = regexp.MustCompile(`(?i)(^|\s)panic:|fatal error:|warning: data race|"level"\s*:\s*"(fatal|panic)"|(^|\s)(fatal|panic)(\s|:)`)

// CheckDaemonLog rejects Go panics, runtime fatal errors, fatal or panic log
// levels, and race detector reports. Ordinary error logs are allowed.
func CheckDaemonLog(log []byte) error {
	for i, line := range bytes.Split(log, []byte{'\n'}) {
		if daemonFailure.Match(line) {
			return fmt.Errorf("daemon failure on log line %d: %s", i+1, line)
		}
	}
	return nil
}
