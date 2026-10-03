#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Called only by run.sh.
set -euo pipefail
umask 077
[[ "$(uname -s)" = Darwin ]]
case "$MAC_SERVER" in
  default) build_tags='' ;;
  smbnext) build_tags=smbnext ;;
  *) echo "MAC_SERVER must be default or smbnext" >&2; exit 1 ;;
esac
git rev-parse HEAD > "$MAC_ARTIFACTS/application-revision"
cp "$MAC_ARTIFACTS/application-revision" "$MAC_ARTIFACTS/harness-revision"
printf '%s\n' "$build_tags" > "$MAC_ARTIFACTS/build-tags"
{
  date -u; sw_vers; uname -a; go version; xcodebuild -version; xcrun --show-sdk-path
  printf 'ImageOS=%s ImageVersion=%s\n' "${ImageOS:-unknown}" "${ImageVersion:-unknown}"
  df -k; diskutil list; diskutil apfs list
} > "$MAC_ARTIFACTS/platform.txt" 2>&1
[[ "$(go env GOVERSION)" = go1.26.3 ]]
export GOENV=off GOTOOLCHAIN=local CGO_ENABLED=1 GOWORK=off GOFLAGS='' GOMAXPROCS=3
# Only this attempt's compiler scratch is disposable; leave global caches/SDKs alone.
compile_cache=$(mktemp -d "$MAC_WORK/compile-cache.XXXXXX")
export GOPATH="$compile_cache/gopath" GOMODCACHE="$compile_cache/modules" GOCACHE="$compile_cache/build"
mkdir "$MAC_BIN"
go build -p 3 -tags "$build_tags" -o "$MAC_BIN/s3-smb" . 2>&1 | tee "$MAC_ARTIFACTS/application-build.log"
"$MAC_BIN/s3-smb" help
"$MAC_BIN/s3-smb" version
go version -m "$MAC_BIN/s3-smb" > "$MAC_ARTIFACTS/native-build.txt"
# Read the same MinIO pin as Linux and the image publisher. Build natively.
minio_release=$(awk -F= '$1 == "ARG MINIO_RELEASE" {print $2}' test/Dockerfile)
minio_revision=$(awk -F= '$1 == "ARG MINIO_COMMIT" {print $2}' test/Dockerfile)
[[ "$minio_release" =~ ^RELEASE\.[0-9TZ-]+$ ]]
[[ "$minio_revision" =~ ^[0-9a-f]{40}$ ]]
printf '%s\n' "$minio_release" > "$MAC_ARTIFACTS/minio-release"
minio_src=$(mktemp -d "$MAC_WORK/minio-source.XXXXXX")
git -C "$minio_src" init
git -C "$minio_src" remote add origin https://github.com/minio/minio.git
git -C "$minio_src" fetch --depth 1 origin "$minio_revision"
git -C "$minio_src" checkout --detach FETCH_HEAD
(cd "$minio_src"; go build -p 3 -o "$MAC_BIN/minio" .) 2>&1 | tee "$MAC_ARTIFACTS/minio-build.log"
printf '%s\n' "$minio_revision" > "$MAC_ARTIFACTS/minio-revision"
go version -m "$MAC_BIN/minio" >> "$MAC_ARTIFACTS/minio-build.log"
go build -p 3 -o "$MAC_BIN/fixture" ./test/macos/fixture
chmod -R u+w "$minio_src" "$compile_cache"
rm -rf "$minio_src" "$compile_cache"
