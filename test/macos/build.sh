#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Called only by run.sh.
set -euo pipefail
umask 077
[[ "$(uname -s)" = Darwin ]]
[[ "$PUBLIC_VERSION" = v0.1.0-rc.7 ]]
[[ "$(git rev-parse 'v0.1.0-rc.7^{}')" = e6e05f032935690617b0338ca369d13d0306a948 ]]
git rev-parse HEAD > "$MAC_ARTIFACTS/harness-revision"
git rev-parse 'v0.1.0-rc.7^{}' > "$MAC_ARTIFACTS/application-source-revision"
git rev-parse v0.1.0-rc.7 > "$MAC_ARTIFACTS/application-tag-object"
printf '%s\n' "$PUBLIC_VERSION" > "$MAC_ARTIFACTS/application-version"
{
  date -u; sw_vers; uname -a; go version; xcodebuild -version; xcrun --show-sdk-path
  printf 'ImageOS=%s ImageVersion=%s\n' "${ImageOS:-unknown}" "${ImageVersion:-unknown}"
  for key in CFBundleVersion CFBundleShortVersionString; do
    printf 'smbfs %s=' "$key"
    /usr/bin/plutil -extract "$key" raw -o - /System/Library/Extensions/smbfs.kext/Contents/Info.plist || true
    echo
  done
  if [[ -f /System/Library/Extensions/smbfs.kext/Contents/MacOS/smbfs ]]; then
    shasum -a 256 /System/Library/Extensions/smbfs.kext/Contents/MacOS/smbfs
  fi
  printf 'capture_buffer_requested_bytes=8388608\ncapture_packet_flush=false\n'
  printf 'capture_effective_descriptor_buffer_bytes=see capture/ready.json BIOCGBLEN\n'
  for key in debug.bpf_bufsize debug.bpf_maxbufsize net.bpf.bufsize net.bpf.maxbufsize; do
    /usr/sbin/sysctl "$key" || true
  done
  df -k; diskutil list; diskutil apfs list
} > "$MAC_ARTIFACTS/platform.txt" 2>&1
[[ "$(go env GOVERSION)" = go1.26.3 ]]
export GOTOOLCHAIN=local CGO_ENABLED=1 GOWORK=off GOFLAGS='' GOMAXPROCS=3
# Only this attempt's compiler scratch is disposable; leave global caches/SDKs alone.
compile_cache=$(mktemp -d "$MAC_WORK/compile-cache.XXXXXX")
export GOPATH="$compile_cache/gopath" GOMODCACHE="$compile_cache/modules" GOCACHE="$compile_cache/build"
mkdir "$MAC_BIN"
# A fresh install from the public Go proxy, not a build of this checkout.
install_root=$(mktemp -d "$MAC_WORK/public-install.XXXXXX")
mkdir "$install_root/empty" "$install_root/bin"
(
  cd "$install_root/empty"
  export GOENV=off GOPATH="$install_root/gopath" GOMODCACHE="$install_root/modules"
  export GOCACHE="$install_root/build" GOBIN="$install_root/bin"
  export GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org
  export GOPRIVATE='' GONOPROXY='' GONOSUMDB=''
  for attempt in 1 2 3; do
    if go install -p 3 "github.com/djosh34/s3-smb@$PUBLIC_VERSION"; then
      break
    fi
    [[ "$attempt" -lt 3 ]]
    sleep 20
  done
  "$GOBIN/s3-smb" help
  "$GOBIN/s3-smb" version
  go version -m "$GOBIN/s3-smb"
) 2>&1 | tee "$MAC_ARTIFACTS/public-install.log"
cp "$install_root/bin/s3-smb" "$MAC_BIN/s3-smb"
printf '%s\n' 'unmodified public rc.7 baseline; no transport observer' > "$MAC_ARTIFACTS/application-variant.txt"
# Separate passive BPF observer; never linked into or substituted for s3-smb.
clang -O2 -Wall -Wextra -Werror test/macos/passive_capture.c -lpcap -o "$MAC_BIN/passive-capture" \
  > "$MAC_ARTIFACTS/capture-build.log" 2>&1
go version -m "$MAC_BIN/s3-smb" > "$MAC_ARTIFACTS/native-build.txt"
shasum -a 256 "$MAC_BIN/s3-smb" > "$MAC_ARTIFACTS/application-sha256.txt"
# Same immutable MinIO source as test/Dockerfile. Native SDK, no Docker/latest.
minio_revision=0d7408fc9969caf07de6a8c3a84f9fbb10a6739e
minio_src=$(mktemp -d "$MAC_WORK/minio-source.XXXXXX")
git -C "$minio_src" init
git -C "$minio_src" remote add origin https://github.com/minio/minio.git
git -C "$minio_src" fetch --depth 1 origin "$minio_revision"
git -C "$minio_src" checkout --detach FETCH_HEAD
(cd "$minio_src"; go build -p 3 -o "$MAC_BIN/minio" .) 2>&1 | tee "$MAC_ARTIFACTS/minio-build.log"
printf '%s\n' "$minio_revision" > "$MAC_ARTIFACTS/minio-revision"
go version -m "$MAC_BIN/minio" >> "$MAC_ARTIFACTS/minio-build.log"
go build -p 3 -o "$MAC_BIN/fixture" ./test/macos/fixture
chmod -R u+w "$install_root" "$minio_src" "$compile_cache"
rm -rf "$install_root" "$minio_src" "$compile_cache"
