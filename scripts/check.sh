#!/usr/bin/env bash
# The local and CI check entry point. Use --gate for phase and release gates.
set -Eeuo pipefail

export S3_SMB_CHECK_MODE=pr
if (( $# == 1 )) && [[ $1 == --gate ]]; then
  export S3_SMB_CHECK_MODE=gate
elif (( $# != 0 )); then
  echo 'Usage: scripts/check.sh [--gate]' >&2
  exit 2
fi
export GOMAXPROCS=${GOMAXPROCS:-2}
export GOFLAGS=${GOFLAGS:--p=2}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
work=$(mktemp -d)
id=s3-smb-$(basename "$work")
network=false
minio=false
runner=false
logs=''
cleanup() {
  status=$?
  trap - EXIT
  for container in runner minio; do
    if [[ ${!container} == true ]]; then
      if ! docker rm -f "$id-$container" >/dev/null; then
        echo "Could not remove $id-$container" >&2
        status=1
      fi
    fi
  done
  if [[ $network == true ]]; then
    if ! docker network rm "$id" >/dev/null; then
      echo "Could not remove network $id" >&2
      status=1
    fi
  fi
  if ! rm -rf "$work"; then
    echo "Could not remove $work" >&2
    status=1
  fi
  if [[ -n $logs ]]; then echo "Daemon logs: $logs"; fi
  exit "$status"
}
trap cleanup EXIT

fuzz_failure() {
  # Seed replay failures can also be fuzz failures. Upload any saved corpus.
  if [[ -n ${GITHUB_OUTPUT:-} ]]; then
    echo 'fuzz_failed=true' >> "$GITHUB_OUTPUT"
  fi
}

# Lint stage. Issue #195 adds golangci-lint, shellcheck and actionlint here.
echo '== Lint =='
go mod tidy -diff
go vet ./...
go vet -tags smbnext ./...
# Vendored code and the frozen SMB server are not ours to format.
find . -type d \( -path './.git' -o -path './internal/juicefs' \
  -o -path './internal/thirdparty' -o -path './internal/smb2' \
  -o -path './internal/smbfs' -o -path './internal/smb-old' \) -prune \
  -o -type f -name '*.go' -print0 > "$work/go-files"
xargs -0 gofmt -l < "$work/go-files" > "$work/unformatted"
if [[ -s $work/unformatted ]]; then
  echo 'Run gofmt on these files:' >&2
  while IFS= read -r file; do printf '%s\n' "$file" >&2; done < "$work/unformatted"
  exit 1
fi
bash test/check_test.sh

echo '== Unit tests and fuzz seed replay =='
if ! go test -count=1 ./...; then
  fuzz_failure
  exit 1
fi
echo '== Race tests =='
if ! go test -race -shuffle=on -count=1 ./...; then
  fuzz_failure
  exit 1
fi

if [[ $S3_SMB_CHECK_MODE == gate ]]; then
  echo '== Fuzz exploration =='
  go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... > "$work/packages"
  while IFS= read -r package; do
    [[ -n $package ]] || continue
    go test -list '^Fuzz' "$package" > "$work/targets"
    while IFS= read -r target; do
      # go test also prints package summaries. Only target names belong here.
      if [[ $target == Fuzz* && $target != *[[:space:]]* ]]; then
        echo "Fuzzing $package/$target"
        if ! go test -run '^$' -fuzz "^${target}$" -fuzztime 1m -parallel 2 "$package"; then
          fuzz_failure
          exit 1
        fi
      fi
    done < "$work/targets"
  done < "$work/packages"
fi

echo '== Docker integration =='
logs=${S3_SMB_TEST_LOGS:-$(mktemp -d)}
mkdir -p "$logs"
logs=$(cd "$logs" && pwd)
hash=$(sha256sum test/Dockerfile)
image=s3-smb-test:${hash%% *}
if ! docker image inspect "$image" >/dev/null 2>&1; then
  docker build -f test/Dockerfile -t "$image" test
fi
docker network create "$id" >/dev/null
network=true
docker create --name "$id-minio" --network "$id" --network-alias minio \
  -e MINIO_ROOT_USER=s3smb-test-access -e MINIO_ROOT_PASSWORD=s3smb-test-secret-only \
  -e MINIO_DOMAIN=minio,transport.test \
  "$image" minio server /data --address :9000 >/dev/null
minio=true
docker start "$id-minio" >/dev/null
# MinIO starts before the tests can create their first bucket.
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$id-minio" curl -fsS http://localhost:9000/minio/health/ready >/dev/null; then
    break
  fi
  if (( attempt == 59 )); then
    docker logs "$id-minio" >&2
    echo 'MinIO did not become ready' >&2
    exit 1
  fi
  sleep 1
done
docker create --name "$id-runner" --network "$id" \
  --add-host transport.test:127.0.0.1 --add-host transport-test.transport.test:127.0.0.1 \
  -v "$root:/src:ro" -v "$logs:/artifacts" \
  -v s3-smb-test-gomod:/go/pkg/mod -v s3-smb-test-gobuild:/root/.cache/go-build \
  -e S3_SMB_E2E_ENDPOINT=http://minio:9000 -e S3_SMB_TEST_ARTIFACTS=/artifacts \
  -e "S3_SMB_CHECK_MODE=$S3_SMB_CHECK_MODE" \
  "$image" bash /src/test/run-linux.sh >/dev/null
runner=true
docker start -a "$id-runner"
# docker start -a does not propagate the container's exit code.
result=$(docker inspect -f '{{.State.ExitCode}}' "$id-runner")
if [[ $result != 0 ]]; then
  fuzz_failure
  exit 1
fi
