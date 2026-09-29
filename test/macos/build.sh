#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Called only by run.sh, in a deadline-supervised process group.
set -euo pipefail
umask 077
[[ "$(uname -s)" = Darwin ]]
[[ "$PUBLIC_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]
git rev-parse HEAD > "$MAC_ARTIFACTS/harness-revision"
printf '%s\n' "$PUBLIC_VERSION" > "$MAC_ARTIFACTS/application-version"
# Record release provenance separately; a harness edit does not re-release the app.
git ls-remote origin "refs/tags/$PUBLIC_VERSION" "refs/tags/$PUBLIC_VERSION^{}" > "$MAC_ARTIFACTS/application-tag-refs"
[[ -s "$MAC_ARTIFACTS/application-tag-refs" ]]
git status --porcelain > "$MAC_ARTIFACTS/source-status"
[[ ! -s "$MAC_ARTIFACTS/source-status" ]]
{
  date -u; sw_vers; uname -a; go version; xcodebuild -version; xcrun --show-sdk-path
  printf 'ImageOS=%s ImageVersion=%s\n' "${ImageOS:-unknown}" "${ImageVersion:-unknown}"
  df -k; diskutil list; diskutil apfs list
} > "$MAC_ARTIFACTS/platform.txt" 2>&1
[[ "$(go env GOVERSION)" = go1.26.3 ]]
export GOTOOLCHAIN=local CGO_ENABLED=1 GOWORK=off GOFLAGS='' GOMAXPROCS=3
# Only this attempt's compiler scratch is disposable; leave global caches/SDKs alone.
compile_cache=$(mktemp -d "$MAC_WORK/compile-cache.XXXXXX")
export GOPATH="$compile_cache/gopath" GOMODCACHE="$compile_cache/modules" GOCACHE="$compile_cache/build"
export MAC_BIN
MAC_BIN=$(mktemp -d "$HOME/s3-smb-native-build.XXXXXX")
printf '%s\n' "$MAC_BIN" > "$MAC_ARTIFACTS/native-build-root"
# A genuinely fresh public install; retain its binary/provenance before removing
# only these exact attempt-generated scratch roots after all builds complete.
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
go version -m "$MAC_BIN/s3-smb" > "$MAC_ARTIFACTS/native-build.txt"
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
# No installed Xcode, simulator, SDK, user content or global cache is removed.
# These are the exact mktemp roots created above, not inferred HOME patterns.
{
  /bin/df -k "$HOME"
  free_before=$(python3 -c 'import shutil; print(shutil.disk_usage(".").free)')
  for scratch in "$install_root" "$minio_src" "$compile_cache"; do
    printf 'remove=%s reason=completed-attempt-build-scratch\n' "$scratch"
    /usr/bin/du -sk "$scratch"
  done
  chmod -R u+w "$install_root" "$minio_src" "$compile_cache"
  rm -rf "$install_root" "$minio_src" "$compile_cache"
  free_after=$(python3 -c 'import shutil; print(shutil.disk_usage(".").free)')
  printf 'free_before_bytes=%s\nfree_after_bytes=%s\nobserved_free_delta_bytes=%s\n' \
    "$free_before" "$free_after" "$((free_after - free_before))"
  /bin/df -k "$HOME"
} > "$MAC_ARTIFACTS/build-scratch-reclamation.log" 2>&1
