//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"context"
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
	dockerfile, err := os.ReadFile(filepath.Join(root, "test/Dockerfile")) //nolint:gosec // Read the checked-out test pin, not an external input.
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
	metadata, err := helpers.Build(h.ctx, root, h.bin, source, os.Getenv("MAC_SERVER"), commit,
		func(ctx context.Context, dir string, args ...string) (string, error) {
			return h.command(ctx, 20*time.Minute, dir, args...)
		})
	h.must(err)
	h.must(os.WriteFile(filepath.Join(h.evidence, "native-build.txt"), []byte(metadata), 0o600)) //nolint:gosec // The path is the run-owned evidence directory; compiler output is only file content.
}
