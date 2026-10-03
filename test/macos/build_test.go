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
	tags, err := helpers.BuildTags(os.Getenv("MAC_SERVER"))
	h.must(err)
	// go test starts in the package directory, not the checkout root.
	root, err := filepath.Abs("../..")
	h.must(err)
	dockerfile, err := os.ReadFile(filepath.Join(root, "test/Dockerfile"))
	h.must(err)
	release, commit, err := helpers.MinIOPin(string(dockerfile))
	h.must(err)
	h.must(os.Mkdir(h.bin, 0o700))
	revision, err := h.command(h.ctx, time.Minute, root, "git", "rev-parse", "HEAD")
	h.must(err)
	for name, value := range map[string]string{"application-revision": revision, "harness-revision": revision, "build-tags": tags + "\n", "minio-release": release + "\n", "minio-revision": commit + "\n"} {
		h.must(os.WriteFile(filepath.Join(h.evidence, name), []byte(value), 0o600))
	}
	for _, args := range [][]string{{"/usr/bin/sw_vers"}, {"uname", "-a"}, {"go", "version"}, {"xcodebuild", "-version"}, {"xcrun", "--show-sdk-path"}, {"/bin/df", "-k"}, {"/usr/sbin/diskutil", "list"}, {"/usr/sbin/diskutil", "apfs", "list"}} {
		h.native(args...)
	}
	if version := strings.TrimSpace(h.native("go", "env", "GOVERSION")); version != "go1.26.3" {
		h.t.Fatal("unexpected Go version", version)
	}
	source, err := os.MkdirTemp(h.work, "minio-source-")
	h.must(err)
	defer func() { h.must(os.RemoveAll(source)) }()
	build := func(dir string, args ...string) string {
		output, err := h.command(h.ctx, 20*time.Minute, dir, args...)
		h.must(err)
		return output
	}
	build(root, "go", "build", "-p", "2", "-tags", tags, "-o", filepath.Join(h.bin, "s3-smb"), ".")
	h.native(filepath.Join(h.bin, "s3-smb"), "help")
	h.native(filepath.Join(h.bin, "s3-smb"), "version")
	metadata := h.native("go", "version", "-m", filepath.Join(h.bin, "s3-smb"))
	h.must(os.WriteFile(filepath.Join(h.evidence, "native-build.txt"), []byte(metadata), 0o600))
	build(source, "git", "init")
	build(source, "git", "remote", "add", "origin", "https://github.com/minio/minio.git")
	build(source, "git", "fetch", "--depth", "1", "origin", commit)
	build(source, "git", "checkout", "--detach", "FETCH_HEAD")
	build(source, "go", "build", "-p", "2", "-o", filepath.Join(h.bin, "minio"), ".")
	h.native("go", "version", "-m", filepath.Join(h.bin, "minio"))
	build(root, "go", "build", "-p", "2", "-o", filepath.Join(h.bin, "fixture"), "./test/macos/fixture")
}
