//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

func (h *harness) build() {
	tags, err := helpers.ServerBuildTags(os.Getenv("MAC_SERVER"), os.Getenv("MAC_PHASE"))
	h.must(err)
	root, err := filepath.Abs("../..")
	h.must(err)
	dockerfile, err := os.ReadFile(filepath.Join(root, "test/Dockerfile")) //nolint:gosec // This is the checked-out test pin.
	h.must(err)
	var release, commit string
	for _, line := range strings.Split(string(dockerfile), "\n") {
		if value, ok := strings.CutPrefix(line, "ARG MINIO_RELEASE="); ok {
			release = value
		}
		if value, ok := strings.CutPrefix(line, "ARG MINIO_COMMIT="); ok {
			commit = value
		}
	}
	if !strings.HasPrefix(release, "RELEASE.") || len(release) <= len("RELEASE.") || strings.Trim(strings.TrimPrefix(release, "RELEASE."), "0123456789TZ-") != "" || len(commit) != 40 || strings.Trim(commit, "0123456789abcdef") != "" {
		h.t.Fatal("invalid MinIO source pin")
	}
	h.must(os.Mkdir(h.bin, 0o700))
	revision := h.run(time.Minute, "git", "-C", root, "rev-parse", "HEAD")
	for name, value := range map[string]string{"application-revision": revision, "harness-revision": revision, "build-tags": tags + "\n", "minio-release": release + "\n", "minio-revision": commit + "\n"} {
		h.must(os.WriteFile(filepath.Join(h.evidence, name), []byte(value), 0o600))
	}
	for _, args := range [][]string{{"/usr/bin/sw_vers"}, {"uname", "-a"}, {"go", "version"}, {"xcodebuild", "-version"}, {"xcrun", "--show-sdk-path"}, {"/bin/df", "-k"}, {"/usr/sbin/diskutil", "list"}, {"/usr/sbin/diskutil", "apfs", "list"}} {
		h.run(2*time.Minute, args...)
	}
	if version := strings.TrimSpace(h.run(2*time.Minute, "go", "env", "GOVERSION")); version != "go1.26.3" {
		h.t.Fatal("unexpected Go version", version)
	}
	source, err := os.MkdirTemp(h.work, "minio-source-")
	h.must(err)
	defer func() {
		if err := os.RemoveAll(source); err != nil {
			h.t.Error(err)
		}
	}()
	application := filepath.Join(h.bin, "s3-smb")
	for _, args := range [][]string{
		{"go", "-C", root, "build", "-p", "2", "-tags", tags, "-o", application, "."},
		{application, "help"},
		{application, "version"},
		{"git", "-C", source, "init"},
		{"git", "-C", source, "remote", "add", "origin", "https://github.com/minio/minio.git"},
		{"git", "-C", source, "fetch", "--depth", "1", "origin", commit},
		{"git", "-C", source, "checkout", "--detach", "FETCH_HEAD"},
		{"go", "-C", source, "build", "-p", "2", "-o", filepath.Join(h.bin, "minio"), "."},
		{"go", "version", "-m", filepath.Join(h.bin, "minio")},
		{"go", "-C", root, "build", "-p", "2", "-o", filepath.Join(h.bin, "fixture"), "./test/macos/fixture"},
		{"go", "-C", root, "build", "-o", filepath.Join(h.bin, "fullsync"), "./test/macos/fullsync"},
	} {
		h.run(20*time.Minute, args...)
	}
	h.must(os.WriteFile(filepath.Join(h.evidence, "native-build.txt"), []byte(h.run(2*time.Minute, "go", "version", "-m", application)), 0o600))
}
