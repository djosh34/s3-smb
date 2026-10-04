//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

const launchdJob = "system/com.s3-smb"

func (h *harness) startLaunchd() int {
	h.stopDaemon(false)
	data, err := os.ReadFile("../../docs/com.s3-smb.plist")
	h.must(err)
	data, err = helpers.LaunchdPlist(data, filepath.Join(h.bin, "s3-smb"), filepath.Join(h.local, "config.yaml"), h.local, h.evidence)
	h.must(err)
	h.launchdPlist = filepath.Join(h.work, "com.s3-smb.plist")
	h.must(os.WriteFile(h.launchdPlist, data, 0o600)) //nolint:gosec // The plist has a fixed name under the run-owned work directory.
	h.run(time.Minute, "/usr/bin/plutil", "-lint", h.launchdPlist)
	// Register cleanup ownership before bootstrap, including a partial startup failure.
	h.run(time.Minute, "/bin/launchctl", "bootstrap", "system", h.launchdPlist)
	return h.launchdReady(0, 1)
}

func (h *harness) launchdReady(previous, starts int) int {
	var pid int
	h.must(h.waitFor("launchd SMB readiness", 3*time.Minute, time.Second, func() (bool, error) {
		text, err := h.try(time.Minute, "/bin/launchctl", "print", launchdJob)
		if err != nil {
			return false, err
		}
		pid, err = helpers.LaunchdPID(text)
		if err != nil {
			return false, err
		}
		var logs string
		for _, name := range []string{"launchd-out.log", "launchd-err.log"} {
			data, readErr := os.ReadFile(filepath.Join(h.evidence, name)) //nolint:gosec // Both names are fixed launchd logs in the run-owned evidence directory.
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			if readErr != nil {
				return false, readErr
			}
			logs += string(data)
		}
		ready, err := helpers.LaunchdServing(logs, starts)
		return ready && pid != 0 && pid != previous, err
	}))
	h.t.Log("launchd-application-ready", "pid", pid, "previous_pid", previous, "starts", starts)
	return pid
}

func (h *harness) unloadLaunchd() error {
	if h.launchdPlist == "" {
		return nil
	}
	_, err := h.try(time.Minute, "/bin/launchctl", "bootout", "system", h.launchdPlist)
	if err != nil {
		return err
	}
	if err := os.Remove(h.launchdPlist); err != nil {
		return err
	}
	h.launchdPlist = ""
	h.t.Log("launchd-job-unloaded")
	return nil
}
