#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Run under the shared heavyweight lock on the development VM. The Docker/CI
# entry point may call this while already holding its own outer lock.
set -eu
cd "$(dirname "$0")/.."
export GOMAXPROCS=2 GOWORK=off CGO_ENABLED=1
# Consumer tags, overlays and vendor mode must not participate in this check.
export GOFLAGS=-mod=readonly
if [ "$(go env GOOS)" != linux ]; then
  echo 'This smoke entry point is the Linux gate; native Mac acceptance is last.' >&2
  exit 1
fi
if [ -f go.work ] || [ -d vendor ]; then
  echo 'workspace/vendor assistance is forbidden' >&2
  exit 1
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
go test -p 2 ./internal/packaging
go list -deps -f '{{.ImportPath}}' . > "$tmp/packages"
if grep -E 'github.com/(hanwen/go-fuse|winfsp/cgofuse|ceph/go-ceph|apple/foundationdb|juicedata/(juicefs|gogfapi))|github.com/macos-fuse-t/go-smb2' "$tmp/packages"; then
  echo 'unexpected unbundled/native package in application graph' >&2
  exit 1
fi
go list -deps -f '{{if .CgoFiles}}{{.ImportPath}}: {{.CgoFiles}} {{.CgoCFLAGS}} {{.CgoLDFLAGS}}{{end}}' .
go build -p 2 -o "$tmp/s3-smb" .
GOBIN="$tmp/bin" go install -p 2 .
"$tmp/bin/s3-smb" help
"$tmp/bin/s3-smb" version
echo 'Linux local build/install smoke passed. This is NOT a public versioned install.'
