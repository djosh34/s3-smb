#!/usr/bin/env bash
# Runs every test, including real SMB + MinIO, in Docker.
# Usage: scripts/test-linux.sh [go test packages or flags]
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
id=s3-smb-test-$$
logs=${S3_SMB_TEST_LOGS:-$(mktemp -d)}
cleanup() {
  docker rm -f "$id-runner" "$id-minio" >/dev/null 2>&1 || true
  docker network rm "$id" >/dev/null 2>&1 || true
  echo "Daemon logs: $logs"
}
trap cleanup EXIT
docker build -f "$root/test/Dockerfile" -t s3-smb-test "$root/test"
docker network create "$id" >/dev/null
docker run -d --name "$id-minio" --network "$id" --network-alias minio \
  -e MINIO_ROOT_USER=s3smb-test-access -e MINIO_ROOT_PASSWORD=s3smb-test-secret-only \
  -e MINIO_DOMAIN=minio,transport.test \
  s3-smb-test minio server /data --address :9000 >/dev/null
docker run --name "$id-runner" --network "$id" \
  --add-host transport.test:127.0.0.1 --add-host transport-test.transport.test:127.0.0.1 \
  -v "$root:/src:ro" -v "$logs:/artifacts" \
  -v s3-smb-test-gomod:/go/pkg/mod -v s3-smb-test-gobuild:/root/.cache/go-build \
  -e S3_SMB_E2E_ENDPOINT=http://minio:9000 -e S3_SMB_TEST_ARTIFACTS=/artifacts \
  -e S3_SMB_LOAD_MIB -e S3_SMB_LOAD_DURATION -e S3_SMB_LOAD_COMPRESSION -e S3_SMB_LOAD_CACHE -e S3_SMB_LOAD_TAIL \
  s3-smb-test bash /src/test/run-linux.sh "$@"
