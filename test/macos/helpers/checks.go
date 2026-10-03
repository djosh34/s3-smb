// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Exclusions is the literal directory list reviewed in the Mac discovery run.
var Exclusions = []string{
	"/System/Volumes/Data/.Spotlight-V100", "/System/Volumes/Data/.TemporaryItems", "/System/Volumes/Data/.fseventsd",
	"/System/Volumes/Data/Applications", "/System/Volumes/Data/Library", "/System/Volumes/Data/MobileSoftwareUpdate",
	"/System/Volumes/Data/Previous Content", "/System/Volumes/Data/System", "/System/Volumes/Data/Volumes",
	"/System/Volumes/Data/cores", "/System/Volumes/Data/mnt", "/System/Volumes/Data/opt", "/System/Volumes/Data/private",
	"/System/Volumes/Data/sw", "/System/Volumes/Data/usr", "/Users/Shared",
	"/Users/runner/.Azure", "/Users/runner/.Trash", "/Users/runner/.android", "/Users/runner/.azure-devops",
	"/Users/runner/.cache", "/Users/runner/.cargo", "/Users/runner/.config", "/Users/runner/.dotnet",
	"/Users/runner/.gradle", "/Users/runner/.homebrew", "/Users/runner/.local", "/Users/runner/.net",
	"/Users/runner/.npm", "/Users/runner/.rustup", "/Users/runner/.ssh", "/Users/runner/.vcpkg", "/Users/runner/.yarn",
	"/Users/runner/Desktop", "/Users/runner/Documents", "/Users/runner/Downloads", "/Users/runner/Library",
	"/Users/runner/Movies", "/Users/runner/actionarchivecache", "/Users/runner/actions-runner", "/Users/runner/bootstrap",
	"/Users/runner/go", "/Users/runner/hostedtoolcache", "/Users/runner/image-generation", "/Users/runner/work",
}

// CheckExclusion validates tmutil's answer for one path.
func CheckExclusion(output string, excluded bool) error {
	marker := "[Included]"
	if excluded {
		marker = "[Excluded]"
	}
	if !strings.HasPrefix(output, marker) {
		return fmt.Errorf("wrong Time Machine exclusion state: %s", output)
	}
	return nil
}

var runningPattern = regexp.MustCompile(`Running\s*=\s*([01])\s*;`)
var copyingPattern = regexp.MustCompile(`BackupPhase\s*=\s*Copying`)
var bytesPattern = regexp.MustCompile(`(?m)^\s*bytes\s*=\s*"?([+\-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+\-]?\d+)?)"?\s*;`)

// Running rejects an unknown native status format.
func Running(text string) (bool, error) {
	match := runningPattern.FindStringSubmatch(text)
	if match == nil {
		return false, errors.New("unknown installed tmutil status format")
	}
	return match[1] == "1", nil
}

// Copying requires a running backup, the Copying phase, and positive copied bytes.
func Copying(text string) bool {
	running, err := Running(text)
	match := bytesPattern.FindStringSubmatch(text)
	if err != nil || !running || !copyingPattern.MatchString(text) || match == nil {
		return false
	}
	number, err := strconv.ParseFloat(match[1], 64)
	return err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) && number > 0
}

// CheckRemoteChange requires a nonempty change in chunk keys or object sizes, not just a count.
func CheckRemoteChange(before, after map[string]int64) error {
	if len(after) == 0 || maps.Equal(before, after) {
		return errors.New("interrupted backup made no remote chunk change")
	}
	return nil
}

// BuildTags selects only the server requested by the workflow.
func BuildTags(server string) (string, error) {
	switch server {
	case "default":
		return "", nil
	case "smbnext":
		return "smbnext", nil
	default:
		return "", errors.New("MAC_SERVER must be default or smbnext")
	}
}

var releasePattern = regexp.MustCompile(`^RELEASE\.[0-9TZ-]+$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// MinIOPin reads the same immutable source pin used by Linux.
func MinIOPin(dockerfile string) (string, string, error) {
	var release, commit string
	for _, line := range strings.Split(dockerfile, "\n") {
		if value, ok := strings.CutPrefix(line, "ARG MINIO_RELEASE="); ok {
			release = value
		}
		if value, ok := strings.CutPrefix(line, "ARG MINIO_COMMIT="); ok {
			commit = value
		}
	}
	if !releasePattern.MatchString(release) || !commitPattern.MatchString(commit) {
		return "", "", errors.New("invalid MinIO source pin")
	}
	return release, commit, nil
}
