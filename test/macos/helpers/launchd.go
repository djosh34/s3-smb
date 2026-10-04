// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"howett.net/plist"
)

// LaunchdPlist fills paths in the shipped job without duplicating its settings.
func LaunchdPlist(data []byte, binary, config, work, evidence string) ([]byte, error) {
	var job map[string]any
	if _, err := plist.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	args, ok := job["ProgramArguments"].([]any)
	if !ok || len(args) != 4 || args[1] != "-c" || args[3] != "serve" {
		return nil, errors.New("unexpected launchd program arguments")
	}
	args[0], args[2] = binary, config
	job["ProgramArguments"] = args
	environment, ok := job["EnvironmentVariables"].(map[string]any)
	if !ok {
		return nil, errors.New("launchd environment is missing")
	}
	environment["HOME"] = work
	job["WorkingDirectory"] = work
	job["StandardOutPath"] = filepath.Join(evidence, "launchd-out.log")
	job["StandardErrorPath"] = filepath.Join(evidence, "launchd-err.log")
	return plist.Marshal(job, plist.XMLFormat)
}

// LaunchdPID reads the running process from launchctl print output, or zero if absent.
func LaunchdPID(text string) (int, error) {
	for _, line := range strings.Split(text, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			pid, err := strconv.Atoi(value)
			if err != nil {
				return 0, err
			}
			if pid <= 0 {
				return 0, errors.New("launchd reported a nonpositive PID")
			}
			return pid, nil
		}
	}
	return 0, nil
}

// LaunchdServing requires a fresh serving log entry and refuses any consent prompt.
func LaunchdServing(text string, starts int) (bool, error) {
	if strings.Contains(text, "Continue? [yes/no]: ") {
		return false, errors.New("launchd start asked for foreground consent")
	}
	return strings.Count(text, `"msg":"SMB serving"`) >= starts, nil
}
