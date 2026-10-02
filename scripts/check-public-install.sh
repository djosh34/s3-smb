#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Installs a published version from the public Go proxy with empty caches.
set -eu
version=${1:?usage: check-public-install.sh vX.Y.Z[-prerelease]}
case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo 'provide an immutable published version, not latest or a branch' >&2; exit 1 ;;
esac
work=$(mktemp -d)
trap 'chmod -R u+w "$work"; rm -rf "$work"' EXIT HUP INT TERM
mkdir -p "$work/empty" "$work/bin"
cd "$work/empty"
export GOENV=off GOWORK=off GOFLAGS= CGO_ENABLED=1 GOMAXPROCS=2
export GOPATH="$work/gopath" GOMODCACHE="$work/modules" GOCACHE="$work/build"
export GOBIN="$work/bin" GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org
export GOPRIVATE= GONOPROXY= GONOSUMDB=
go version
go install -p 2 "github.com/djosh34/s3-smb@$version"
"$work/bin/s3-smb" help
"$work/bin/s3-smb" version
go version -m "$work/bin/s3-smb"
echo "Public fresh-cache install passed: $version"
