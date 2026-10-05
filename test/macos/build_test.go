//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// build builds s3-smb from this checkout, MinIO from the commit pinned in
// test/Dockerfile and the fullsync probe, and saves their versions as evidence.
func (h *harness) build() {
	root, err := filepath.Abs("../..")
	h.must(err)
	dockerfile, err := os.ReadFile("../../test/Dockerfile")
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
	for name, value := range map[string]string{"application-revision": revision, "harness-revision": revision, "minio-release": release + "\n", "minio-revision": commit + "\n"} {
		h.must(h.evidenceDir.WriteFile(name, []byte(value), 0o600))
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
	h.download("go", "-C", root, "mod", "download")
	for _, args := range [][]string{
		{"go", "-C", root, "build", "-p", "2", "-o", application, "."},
		{application, "help"},
		{application, "version"},
		{"git", "-C", source, "init"},
		{"git", "-C", source, "remote", "add", "origin", "https://github.com/minio/minio.git"},
	} {
		h.run(20*time.Minute, args...)
	}
	h.download("git", "-C", source, "fetch", "--depth", "1", "origin", commit)
	h.run(time.Minute, "git", "-C", source, "checkout", "--detach", "FETCH_HEAD")
	h.download("go", "-C", source, "mod", "download")
	for _, args := range [][]string{
		{"go", "-C", source, "build", "-p", "2", "-o", filepath.Join(h.bin, "minio"), "."},
		{"go", "version", "-m", filepath.Join(h.bin, "minio")},
		{"go", "-C", root, "build", "-o", filepath.Join(h.bin, "fullsync"), "./test/macos/fullsync"},
	} {
		h.run(20*time.Minute, args...)
	}
	h.must(h.evidenceDir.WriteFile("native-build.txt", []byte(h.run(2*time.Minute, "go", "version", "-m", application)), 0o600))
}

// download runs a command that fetches modules or sources. It makes up to
// three attempts when the output shows a transient network failure.
func (h *harness) download(args ...string) {
	h.t.Helper()
	for attempt := 1; ; attempt++ {
		output, err := h.try(20*time.Minute, args...)
		if err == nil {
			return
		}
		h.t.Logf("download attempt %d failed: %v\n%s", attempt, err, output)
		if attempt == 3 || !transientNetworkFailure(output) {
			h.t.Fatal(err)
		}
		h.pause(2 * time.Second)
	}
}

func transientNetworkFailure(output string) bool {
	output = strings.ToLower(output)
	for _, message := range []string{
		"could not resolve host", "could not resolve proxy", "no such host",
		"temporary failure in name resolution", "connection reset", "connection refused",
		"network is unreachable", "i/o timeout", "tls handshake timeout",
		"connection timed out", "operation timed out", "unexpected eof",
		"failed to connect to", "couldn't connect to server", "could not connect to server",
		"rpc failed; curl 18", "rpc failed; curl 28", "rpc failed; curl 52",
		"rpc failed; curl 55", "rpc failed; curl 56", "early eof",
		"unexpected disconnect while reading sideband packet",
		"500 internal server error", "502 bad gateway", "503 service unavailable", "504 gateway timeout",
		"the requested url returned error: 500", "the requested url returned error: 502",
		"the requested url returned error: 503", "the requested url returned error: 504",
	} {
		if strings.Contains(output, message) {
			return true
		}
	}
	return false
}
