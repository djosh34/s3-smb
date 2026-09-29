#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
set -euo pipefail
umask 077
export MAC_DEADLINE_EPOCH=$(( $(date +%s) + 330 * 60 ))
cd "$(dirname "$0")/../.."
repo=$PWD
: "${MAC_ARTIFACTS:?absolute evidence directory required}"
: "${MAC_WORK:?absolute task-owned work directory required}"
: "${PUBLIC_VERSION:?immutable published version required}"
[[ "$MAC_ARTIFACTS" = /* && "$MAC_WORK" = /* && "$MAC_WORK" != / ]]
[[ ! -e "$MAC_WORK" && ! -e "$MAC_ARTIFACTS" ]]
mkdir -m 700 "$MAC_WORK" "$MAC_ARTIFACTS"
exec > >(tee "$MAC_ARTIFACTS/entrypoint.log") 2>&1
record_exit() {
  local rc=$?
  printf '%s\n' "$rc" > "$MAC_ARTIFACTS/exit-status"
  if /bin/df -k > "$MAC_ARTIFACTS/exit-capacity.log" 2>&1; then
    printf '0\n' > "$MAC_ARTIFACTS/exit-capacity-status"
  else
    printf '%s\n' "$?" > "$MAC_ARTIFACTS/exit-capacity-status"
  fi
  exit "$rc"
}
trap record_exit EXIT
[[ "$(uname -s)" = Darwin ]]
[[ "$PUBLIC_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]
sha=$(git rev-parse HEAD)
[[ "$(git ls-remote origin "refs/tags/$PUBLIC_VERSION" | cut -f1)" = "$sha" ]]
printf '%s\n' "$sha" > "$MAC_ARTIFACTS/revision"
git status --porcelain > "$MAC_ARTIFACTS/source-status"
[[ ! -s "$MAC_ARTIFACTS/source-status" ]]
{
  date -u; sw_vers; uname -a; go version; xcodebuild -version; xcrun --show-sdk-path
  printf 'ImageOS=%s ImageVersion=%s\n' "${ImageOS:-unknown}" "${ImageVersion:-unknown}"
  df -k; diskutil list; diskutil apfs list
} > "$MAC_ARTIFACTS/platform.txt" 2>&1
[[ "$(go env GOVERSION)" = go1.26.3 ]]
export GOTOOLCHAIN=local CGO_ENABLED=1 GOWORK=off GOFLAGS='' GOMAXPROCS=3
export MAC_BIN
MAC_BIN=$(mktemp -d "$HOME/s3-smb-native-build.XXXXXX")
printf '%s\n' "$MAC_BIN" > "$MAC_ARTIFACTS/native-build-root"
go build -p 3 -o "$MAC_BIN/s3-smb-checkout" .
# Genuine public installation: empty external cwd and all-new caches. Preserve
# these ordinary build contents; do not delete/exclude them to shrink the source.
install_root=$(mktemp -d "$HOME/s3-smb-public-install.XXXXXX")
printf '%s\n' "$install_root" > "$MAC_ARTIFACTS/public-install-root"
mkdir "$install_root/empty" "$install_root/bin"
(
  cd "$install_root/empty"
  export GOENV=off GOPATH="$install_root/gopath" GOMODCACHE="$install_root/modules"
  export GOCACHE="$install_root/build" GOBIN="$install_root/bin"
  export GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org
  export GOPRIVATE='' GONOPROXY='' GONOSUMDB=''
  go install -p 3 "github.com/djosh34/s3-smb@$PUBLIC_VERSION"
  "$GOBIN/s3-smb" help
  "$GOBIN/s3-smb" version
  go version -m "$GOBIN/s3-smb"
) > "$MAC_ARTIFACTS/public-install.log" 2>&1
cp "$install_root/bin/s3-smb" "$MAC_BIN/s3-smb"
go version -m "$MAC_BIN/s3-smb-checkout" > "$MAC_ARTIFACTS/native-build.txt"
# Same immutable MinIO source as test/Dockerfile. Native SDK, no Docker/latest.
minio_revision=0d7408fc9969caf07de6a8c3a84f9fbb10a6739e
minio_src=$(mktemp -d "$HOME/s3-smb-minio-source.XXXXXX")
git -C "$minio_src" init
git -C "$minio_src" remote add origin https://github.com/minio/minio.git
git -C "$minio_src" fetch --depth 1 origin "$minio_revision"
git -C "$minio_src" checkout --detach FETCH_HEAD
[[ "$(git -C "$minio_src" rev-parse HEAD)" = "$minio_revision" ]]
(cd "$minio_src"; go build -p 3 -o "$MAC_BIN/minio" .) > "$MAC_ARTIFACTS/minio-build.log" 2>&1
printf '%s\n' "$minio_revision" > "$MAC_ARTIFACTS/minio-revision"
go version -m "$MAC_BIN/minio" >> "$MAC_ARTIFACTS/minio-build.log"
go build -p 3 -o "$MAC_BIN/fixture" ./test/macos/fixture
# sudo is ordinary administration, NOT proof of Full Disk Access. Every native
# operation below must actually succeed; no TCC/SIP modification is attempted.
sudo -n /usr/bin/env "PATH=$PATH" "HOME=$HOME" "MAC_WORK=$MAC_WORK" \
  "MAC_ARTIFACTS=$MAC_ARTIFACTS" "MAC_RUNNER_HOME=$HOME" "MAC_DEADLINE_EPOCH=$MAC_DEADLINE_EPOCH" \
  "MAC_BIN=$MAC_BIN" PYTHONDONTWRITEBYTECODE=1 \
  python3 "$repo/test/macos/acceptance.py"
