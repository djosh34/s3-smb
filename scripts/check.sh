#!/usr/bin/env bash
# The local and CI check entry point. Use --gate for the release gate. Without a
# part it runs lint, unit tests, fuzzing in gate mode and the Docker integration
# one after another. CI runs each part as its own job. fuzz takes an optional
# shard, like fuzz 2/4, which runs every fourth fuzz target from the second on.
# integration takes one too: shard 1 runs the integration tests, and the others
# share the break tests, as test/run-linux.sh says.
set -Eeuo pipefail

usage() {
  echo 'Usage: scripts/check.sh [--gate] [lint | unit | fuzz [SHARD/SHARDS] | integration [SHARD/SHARDS]]' >&2
  exit 2
}
export S3_SMB_CHECK_MODE=pr
if (( $# > 0 )) && [[ $1 == --gate ]]; then
  export S3_SMB_CHECK_MODE=gate
  shift
fi
part=all shard=1 shards=1
case "$#:${1:-}" in
  0:) ;;
  1:lint | 1:unit | 1:fuzz | 1:integration) part=$1 ;;
  2:fuzz | 2:integration)
    if [[ ! $2 =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] || (( BASH_REMATCH[1] > BASH_REMATCH[2] )); then
      usage
    fi
    part=$1 shard=${BASH_REMATCH[1]} shards=${BASH_REMATCH[2]} ;;
  *) usage ;;
esac
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
work=$(mktemp -d)
id=s3-smb-$(basename "$work")
network=false
containers=()
logs=''
cleanup() {
  status=$?
  trap - EXIT
  for container in "${containers[@]}"; do
    if ! docker rm -f "$container" >/dev/null; then
      echo "Could not remove $container" >&2
      status=1
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

check_lint() {
  echo '== Lint =='
  tools=$(bash scripts/lint-tools.sh)
  "$tools/golangci-lint" config verify
  "$tools/golangci-lint" run ./...
  GOOS=darwin "$tools/golangci-lint" run --build-tags macos ./test/macos/...
  find . -type d -path './.git' -prune -o -type f -name '*.sh' -print0 > "$work/shell-files"
  xargs -0 -r "$tools/shellcheck" < "$work/shell-files"
  # actionlint also checks inline shell with our pinned shellcheck, not PATH tools.
  find .github/workflows -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 > "$work/workflows"
  xargs -0 -r "$tools/actionlint" -shellcheck "$tools/shellcheck" -pyflakes '' < "$work/workflows"
  go mod tidy -diff
  go vet ./...
  go vet -tags testcopies ./internal/engine
  GOOS=darwin go vet -tags macos ./test/macos/...
  find . -type d -path './.git' -prune -o -type f -name '*.go' -print0 > "$work/go-files"
  xargs -0 gofmt -l < "$work/go-files" > "$work/unformatted"
  if [[ -s $work/unformatted ]]; then
    echo 'Run gofmt on these files:' >&2
    while IFS= read -r file; do printf '%s\n' "$file" >&2; done < "$work/unformatted"
    exit 1
  fi
  bash test/check_test.sh
  bash test/lint_tools_test.sh
  bash test/lint_config_test.sh "$tools"
  bash test/minio/publish_test.sh
}

check_unit() {
  echo '== Unit tests and fuzz seed replay =='
  if ! go test -race -shuffle=on -count=1 ./...; then
    fuzz_failure
    exit 1
  fi
}

check_fuzz() {
  echo "== Fuzz exploration, shard $shard of $shards =="
  go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... > "$work/packages"
  index=0
  while IFS= read -r package; do
    [[ -n $package ]] || continue
    go test -list '^Fuzz' "$package" > "$work/targets"
    while IFS= read -r target; do
      # go test also prints package summaries. Only target names belong here.
      if [[ $target == Fuzz* && $target != *[[:space:]]* ]]; then
        index=$((index + 1))
        if (( (index - 1) % shards != shard - 1 )); then continue; fi
        echo "Fuzzing $package/$target"
        if ! go test -run '^$' -fuzz "^${target}$" -fuzztime 1m -parallel 2 "$package"; then
          fuzz_failure
          exit 1
        fi
      fi
    done < "$work/targets"
  done < "$work/packages"
}

check_integration() {
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
  containers+=("$id-minio")
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
    -e "S3_SMB_CHECK_MODE=$S3_SMB_CHECK_MODE" -e S3_SMB_CHAOS_SEED -e "S3_SMB_SHARD=$shard/$shards" \
    "$image" bash /src/test/run-linux.sh >/dev/null
  containers=("$id-runner" "${containers[@]}")
  docker start -a "$id-runner"
}

if [[ $part == all ]]; then
  check_lint
  check_unit
  if [[ $S3_SMB_CHECK_MODE == gate ]]; then check_fuzz; fi
  check_integration
else
  "check_$part"
fi
