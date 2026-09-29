#!/usr/bin/env bash
# The sole local/CI Linux entry point. No production credentials or host ports.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
mode=${1:-suite}
if (( $# > 0 )); then shift; fi
case "$mode" in suite|release|unit|race|e2e) ;; *) echo 'usage: scripts/test-linux.sh [suite|release|unit|race|e2e] [go packages...]' >&2; exit 2;; esac
exec 9>/tmp/s3-smb-heavy.lock
flock 9
id="s3-smb-test-$(date -u +%Y%m%dT%H%M%SZ)-$$"
artifacts=${S3_SMB_TEST_ARTIFACTS:-/tmp/$id-artifacts}
mkdir -p "$artifacts"
artifacts=$(cd "$artifacts" && pwd)
image=s3-smb-test:go1.26.3-minio-0d7408fc
network=$id
volume=$id-data
runner=$id-runner
minio=$id-minio
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
docker run -d --name "$minio" --network "$network" --network-alias minio \
  --memory=1g --cpus=2 -e MINIO_ROOT_USER=s3smb-test-access \
  -e MINIO_ROOT_PASSWORD=s3smb-test-secret-only -e MINIO_DOMAIN=minio,transport.minio \
  -v "$volume:/data" "$image" minio server /data --address :9000 >/dev/null
# No host HOME/AWS settings or data volumes are inherited. Source is read-only.
docker run --name "$runner" --network "$network" --memory=5g --cpus=2 \
  -v "$root:/src:ro" -v "$artifacts:/artifacts" \
  -e S3_SMB_E2E_ENDPOINT=http://minio:9000 -e S3_SMB_TEST_ARTIFACTS=/artifacts \
  -e S3_SMB_TEST_INJECT_FAILURE="${S3_SMB_TEST_INJECT_FAILURE:-}" \
  "$image" bash /src/test/run-linux.sh "$mode" "$@" 2>&1 | tee "$artifacts/console.log"
