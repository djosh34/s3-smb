// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"path/filepath"
)

// BuildRunner runs one compiler or source command and returns its output.
type BuildRunner func(context.Context, string, ...string) (string, error)

// Build compiles only the checkout's selected server, then pinned MinIO and the bucket fixture.
// A failed build or metadata read stops before fetching MinIO.
func Build(ctx context.Context, root, bin, source, server, commit string, run BuildRunner) (string, error) {
	tags, err := BuildTags(server)
	if err != nil {
		return "", err
	}
	application := filepath.Join(bin, "s3-smb")
	steps := []struct {
		dir  string
		args []string
	}{
		{root, []string{"go", "build", "-p", "2", "-tags", tags, "-o", application, "."}},
		{root, []string{application, "help"}},
		{root, []string{application, "version"}},
		{root, []string{"go", "version", "-m", application}},
		{source, []string{"git", "init"}},
		{source, []string{"git", "remote", "add", "origin", "https://github.com/minio/minio.git"}},
		{source, []string{"git", "fetch", "--depth", "1", "origin", commit}},
		{source, []string{"git", "checkout", "--detach", "FETCH_HEAD"}},
		{source, []string{"go", "build", "-p", "2", "-o", filepath.Join(bin, "minio"), "."}},
		{source, []string{"go", "version", "-m", filepath.Join(bin, "minio")}},
		{root, []string{"go", "build", "-p", "2", "-o", filepath.Join(bin, "fixture"), "./test/macos/fixture"}},
	}
	var metadata string
	for index, step := range steps {
		output, err := run(ctx, step.dir, step.args...)
		if err != nil {
			return "", err
		}
		if index == 3 {
			metadata = output
		}
	}
	return metadata, nil
}
