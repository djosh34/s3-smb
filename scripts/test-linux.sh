#!/usr/bin/env bash
# The sole local/CI Linux entry point. No production credentials or host ports.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
mode=${1:-suite}
if (( $# > 0 )); then shift; fi
case "$mode" in suite|release|unit|race|e2e|transport) ;; *) echo 'usage: scripts/test-linux.sh [suite|release|unit|race|e2e|transport] [go packages...]' >&2; exit 2;; esac
id="s3-smb-test-$(date -u +%Y%m%dT%H%M%SZ)-$$"
artifacts=${S3_SMB_TEST_ARTIFACTS:-/tmp/$id-artifacts}
mkdir -p "$artifacts"
artifacts=$(cd "$artifacts" && pwd)
# Exercise our actual failure trap before taking the parent build lock. This
# uses exactly the same fixture and runner, not a mock Docker executable.
if [[ $mode == release ]]; then
  proof="$artifacts/failure-proof"
  if S3_SMB_TEST_INJECT_FAILURE=1 S3_SMB_TEST_ARTIFACTS="$proof" "$root/scripts/test-linux.sh" unit ./test/coverage; then
    echo 'Expected deliberately failed fixture to exit nonzero.' >&2; exit 1
  else
    test "$?" = 1
  fi
  test "$(<"$proof/exit-status")" = 1
  grep -qx $'unit\tPASS' "$proof/phases.tsv"
  for file in minio.log runner.log containers.json injected-failure.txt retained-volume.txt; do test -s "$proof/$file"; done
  retained=$(awk '/^Failure:/ { print $NF }' "$proof/retained-volume.txt")
  test "$(docker volume inspect "$retained" --format '{{index .Labels "s3-smb.disposable"}}')" = true
  # Delete only the exact labeled volume whose preservation we just verified.
  docker volume rm "$retained" >/dev/null
  printf '%s\n' '{"Action":"pass","Package":"github.com/djosh34/s3-smb/test/harness","Test":"TestFailureArtifacts"}' >"$artifacts/harness.json"
fi
exec 9>/tmp/s3-smb-heavy.lock
flock 9
image=s3-smb-test:go1.26.3-minio-0d7408fc
network=$id
volume=$id-data
runner=$id-runner
minio=$id-minio
# Go caches are content-addressed and contain build/dependency material only.
# Keep these across local runs; never reuse MinIO, metadata or application caches.
module_cache=s3-smb-test-go1263-modules
build_cache=s3-smb-test-go1263-build
cleanup() {
  status=$?
  trap - EXIT
  docker logs "$minio" >"$artifacts/minio.log" 2>&1 || true
  docker logs "$runner" >"$artifacts/runner.log" 2>&1 || true
  docker inspect "$minio" "$runner" >"$artifacts/containers.json" 2>&1 || true
  printf '%s\n' "$status" >"$artifacts/exit-status"
  docker rm -f "$runner" "$minio" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  if [[ $status == 0 ]]; then
    docker volume rm "$volume" >/dev/null 2>&1 || true
  else
    printf 'Failure: disposable MinIO volume retained: %s\nRemove explicitly: docker volume rm %q\n' "$volume" "$volume" | tee "$artifacts/retained-volume.txt"
  fi
  printf 'Artifacts: %s\n' "$artifacts"
  exit "$status"
}
trap cleanup EXIT
{
  date -u --iso-8601=seconds
  uname -a
  git -C "$root" rev-parse HEAD
  git -C "$root" status --short
  git -C "$root" diff --binary
  docker version
} >"$artifacts/environment.txt" 2>&1
# A dirty shared checkout is explicitly recorded, never claimed as a tested commit.
docker build --progress=plain -f "$root/test/Dockerfile" -t "$image" "$root/test" 2>&1 | tee "$artifacts/image-build.log"
docker image inspect "$image" >"$artifacts/image.json"
docker network create "$network" >/dev/null
docker volume create --label s3-smb.disposable=true "$volume" >/dev/null
docker volume create --label s3-smb.build-cache=true "$module_cache" >/dev/null
docker volume create --label s3-smb.build-cache=true "$build_cache" >/dev/null
docker run -d --name "$minio" --network "$network" --network-alias minio \
  --memory=1g --cpus=2 -e MINIO_ROOT_USER=s3smb-test-access \
  -e MINIO_ROOT_PASSWORD=s3smb-test-secret-only -e MINIO_DOMAIN=minio,transport.test \
  -v "$volume:/data" "$image" minio server /data --address :9000 >/dev/null
# No host HOME/AWS settings or data volumes are inherited. Source is read-only.
docker run --name "$runner" --network "$network" --memory=5g --cpus=2 \
  -v "$root:/src:ro" -v "$artifacts:/artifacts" \
  -v "$module_cache:/go/pkg/mod" -v "$build_cache:/root/.cache/go-build" \
  -e S3_SMB_E2E_ENDPOINT=http://minio:9000 -e S3_SMB_TEST_ARTIFACTS=/artifacts \
  -e S3_SMB_ARTIFACT_UID="$(id -u)" -e S3_SMB_ARTIFACT_GID="$(id -g)" \
  -e S3_SMB_TEST_INJECT_FAILURE="${S3_SMB_TEST_INJECT_FAILURE:-}" \
  "$image" bash -c 'cp /src/test/run-linux.sh /tmp/run-linux.sh; exec bash /tmp/run-linux.sh "$@"' runner "$mode" "$@" 2>&1 | tee "$artifacts/console.log"
