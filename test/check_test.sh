#!/usr/bin/env bash
# Exercise orchestration without building Go packages or starting Docker.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/repo/scripts" "$fixture/repo/test/minio" "$fixture/bin" "$fixture/logs" \
  "$fixture/repo/.github/workflows"
for directory in .git internal/juicefs internal/thirdparty internal/smb-old; do
  mkdir -p "$fixture/repo/$directory"
  touch "$fixture/repo/$directory/ignored.go"
done
touch "$fixture/repo/our file.go"
cp "$root/scripts/check.sh" "$fixture/repo/scripts/check.sh"
printf 'FROM scratch\n' > "$fixture/repo/test/Dockerfile"
cat > "$fixture/repo/test/check_test.sh" <<'SCRIPT'
echo script-tests >> "$CHECK_TEST_COMMANDS"
SCRIPT
cat > "$fixture/repo/test/minio/publish_test.sh" <<'SCRIPT'
echo pin-tests >> "$CHECK_TEST_COMMANDS"
SCRIPT
cat > "$fixture/repo/test/lint_tools_test.sh" <<'SCRIPT'
echo tool-tests >> "$CHECK_TEST_COMMANDS"
SCRIPT
cat > "$fixture/repo/test/lint_config_test.sh" <<'SCRIPT'
echo config-tests >> "$CHECK_TEST_COMMANDS"
SCRIPT
cat > "$fixture/repo/scripts/lint-tools.sh" <<'SCRIPT'
printf '%s\n' "$CHECK_TEST_TOOLS"
SCRIPT
touch "$fixture/repo/.github/workflows/check.yml" "$fixture/repo/.github/workflows/other.yaml"
export CHECK_TEST_TOOLS="$fixture/bin"

cat > "$fixture/bin/stub" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
command=${0##*/}
command=${command%-stub}
if [[ ${GOMAXPROCS-unset} != "$CHECK_TEST_GOMAXPROCS" || ${GOFLAGS-unset} != "$CHECK_TEST_GOFLAGS" ]]; then
  echo "Go settings changed: GOMAXPROCS=${GOMAXPROCS-unset} GOFLAGS=${GOFLAGS-unset}" >&2
  exit 1
fi
printf '%s [%s] %s\n' "$command" "$S3_SMB_CHECK_MODE" "$*" >> "$CHECK_TEST_COMMANDS"
if [[ ${GOOS:-} == darwin ]]; then
  printf 'darwin %s %s\n' "$command" "$*" >> "$CHECK_TEST_COMMANDS"
fi
if [[ ${CHECK_TEST_FAIL:-} == "$command $*" ]]; then exit 17; fi
if [[ -n ${CHECK_TEST_FAIL_PREFIX:-} && "$command $*" == "$CHECK_TEST_FAIL_PREFIX"* ]]; then exit 17; fi
case "$command $*" in
  'go test -race -shuffle=on -count=1 -timeout=30m ./...')
    [[ ${S3_SMB_E2E_BINARY:-} == /tmp/s3-smb ]] ;;
  'go list '*) printf 'example/one\n\nexample/two\n' ;;
  "go test -list ^Fuzz example/one")
    if [[ ${CHECK_TEST_TARGETS:-yes} == yes ]]; then printf 'FuzzFirst\nFuzzSecond\nFuzz\nFuzz日本\n'; fi
    printf 'ok example/one 0.01s\n' ;;
  "go test -list ^Fuzz example/two")
    if [[ ${CHECK_TEST_TARGETS:-yes} == yes ]]; then printf 'FuzzOther\n'; fi
    printf '? example/two [no test files]\n' ;;
  'gofmt '*) printf '%s' "${CHECK_TEST_UNFORMATTED:-}" ;;
  'docker image inspect '*) [[ ${CHECK_TEST_CACHED:-no} == yes ]] ;;
  'docker start -a '*) exit "${CHECK_TEST_CONTAINER_EXIT:-0}" ;;
esac
STUB
chmod +x "$fixture/bin/stub"
for command in go gofmt docker sleep golangci-lint shellcheck actionlint; do
  ln -s stub "$fixture/bin/$command"
done
export PATH="$fixture/bin:$PATH"
export CHECK_TEST_COMMANDS="$fixture/commands"
export S3_SMB_TEST_LOGS="$fixture/logs"
export GITHUB_OUTPUT="$fixture/github-output"
unset CHECK_TEST_FAIL CHECK_TEST_FAIL_PREFIX CHECK_TEST_CACHED CHECK_TEST_TARGETS CHECK_TEST_UNFORMATTED CHECK_TEST_CONTAINER_EXIT
unset GOMAXPROCS GOFLAGS
export CHECK_TEST_GOMAXPROCS=unset CHECK_TEST_GOFLAGS=unset

fail() { echo "check.sh test failed: $*" >&2; exit 1; }
contains() { grep -F -- "$1" "$CHECK_TEST_COMMANDS" >/dev/null || fail "missing command: $1"; }
absent() { if grep -F -- "$1" "$CHECK_TEST_COMMANDS" >/dev/null; then fail "unexpected command: $1"; fi; }
run_check() {
  : > "$CHECK_TEST_COMMANDS"
  : > "$GITHUB_OUTPUT"
  if bash "$fixture/repo/scripts/check.sh" "$@" > "$fixture/output" 2>&1; then
    result=0
  else
    result=$?
  fi
}
succeeds() { [[ $result == 0 ]] || fail "expected success, got $result (see $fixture/output)"; }
fails() { [[ $result != 0 ]] || fail 'expected failure'; }

# PR mode replays seeds, checks both build selections, and runs integration.
export S3_SMB_CHECK_MODE=gate
run_check
succeeds
contains 'golangci-lint [pr] config verify'
contains 'golangci-lint [pr] run ./...'
contains 'golangci-lint [pr] run --build-tags smbnext ./...'
contains 'shellcheck [pr] '
contains './scripts/check.sh'
contains './test/minio/publish_test.sh'
contains "actionlint [pr] -shellcheck $fixture/bin/shellcheck -pyflakes "
contains '.github/workflows/check.yml'
contains '.github/workflows/other.yaml'
contains 'go [pr] mod tidy -diff'
contains 'go [pr] vet ./...'
contains 'go [pr] vet -tags smbnext ./...'
contains 'gofmt [pr] -l ./our file.go'
absent 'ignored.go'
contains 'script-tests'
contains 'tool-tests'
contains 'config-tests'
contains 'pin-tests'
contains 'darwin golangci-lint run --build-tags macos ./test/macos/...'
contains 'darwin go vet -tags macos ./test/macos/...'
contains 'go [pr] test -count=1 ./...'
absent 'go [pr] test -race '
absent 'docker [pr] inspect '
[[ $(grep -c 'go \[pr\] test -count=1 ./...' "$CHECK_TEST_COMMANDS") == 1 ]] || fail 'unit tests ran more than once'
contains 'docker [pr] build -f test/Dockerfile -t s3-smb-test:'
contains '-e S3_SMB_CHECK_MODE=pr'
contains 'bash /src/test/run-linux.sh'
contains 'docker [pr] rm -f'
contains 'docker [pr] network rm'
absent '-fuzz '
absent 'go [pr] list '
image=$(grep 'docker \[pr\] build' "$CHECK_TEST_COMMANDS" | awk '{print $7}')
expected=$(sha256sum "$fixture/repo/test/Dockerfile")
[[ $image == "s3-smb-test:${expected%% *}" ]] || fail 'image tag does not match Dockerfile hash'

# Cached images are reused; changed Dockerfiles get a different tag.
export CHECK_TEST_CACHED=yes
run_check
succeeds
absent ' build '
contains "docker [pr] image inspect $image"
unset CHECK_TEST_CACHED
printf '# changed\n' >> "$fixture/repo/test/Dockerfile"
run_check
succeeds
absent "-t $image "

# Gate discovery runs all targets in their own packages, with exact limits.
run_check --gate
succeeds
contains 'darwin golangci-lint run --build-tags macos ./test/macos/...'
contains 'darwin go vet -tags macos ./test/macos/...'
contains 'go [gate] test -count=1 ./...'
contains 'go [gate] test -run ^$ -fuzz ^FuzzFirst$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^FuzzSecond$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^FuzzOther$ -fuzztime 1m -parallel 2 example/two'
contains 'go [gate] test -run ^$ -fuzz ^Fuzz$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^Fuzz日本$ -fuzztime 1m -parallel 2 example/one'
contains '-e S3_SMB_CHECK_MODE=gate'
[[ $(grep -c -- ' -fuzz ' "$CHECK_TEST_COMMANDS") == 5 ]] || fail 'wrong fuzz target count'
export CHECK_TEST_TARGETS=no
run_check --gate
succeeds
absent ' -fuzz '
contains 'docker [gate] start -a'
unset CHECK_TEST_TARGETS

# Neither mode sets defaults or changes caller-supplied Go settings.
for setting in supplied empty; do
  if [[ $setting == supplied ]]; then
    export GOMAXPROCS=7 GOFLAGS='-mod=readonly -p=5'
  else
    export GOMAXPROCS='' GOFLAGS=''
  fi
  export CHECK_TEST_GOMAXPROCS=$GOMAXPROCS CHECK_TEST_GOFLAGS=$GOFLAGS
  run_check
  succeeds
  run_check --gate
  succeeds
  contains 'go [gate] test -run ^$ -fuzz ^FuzzFirst$ -fuzztime 1m -parallel 2 example/one'
done
unset GOMAXPROCS GOFLAGS
export CHECK_TEST_GOMAXPROCS=unset CHECK_TEST_GOFLAGS=unset

# Failures stop later stages. Seed and exploration failures request artifacts.
for command in 'golangci-lint config verify' 'golangci-lint run ./...' \
  'golangci-lint run --build-tags smbnext ./...' \
  'golangci-lint run --build-tags macos ./test/macos/...' \
  'go mod tidy -diff' 'go vet ./...' 'go vet -tags smbnext ./...' \
  'go vet -tags macos ./test/macos/...' \
  'go test -count=1 ./...' \
  'go list -f {{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}} ./...' \
  'go test -list ^Fuzz example/one' \
  'go test -list ^Fuzz example/two' \
  'go test -run ^$ -fuzz ^FuzzFirst$ -fuzztime 1m -parallel 2 example/one' \
  'go test -run ^$ -fuzz ^FuzzOther$ -fuzztime 1m -parallel 2 example/two'; do
  export CHECK_TEST_FAIL=$command
  run_check --gate
  fails
  absent 'docker [gate] network create'
  case "$command" in
    'go test -count=1 ./...'|'go test -run '*)
      grep -Fx 'fuzz_failed=true' "$GITHUB_OUTPUT" >/dev/null || fail 'missing fuzz artifact signal' ;;
  esac
done
unset CHECK_TEST_FAIL
for prefix in 'shellcheck ' 'actionlint '; do
  export CHECK_TEST_FAIL_PREFIX=$prefix
  run_check
  fails
  absent 'go [pr] mod tidy'
  absent 'docker [pr] network create'
done
unset CHECK_TEST_FAIL_PREFIX
export CHECK_TEST_UNFORMATTED=$'./bad.go\n'
run_check
fails
absent 'go [pr] test '
grep -F './bad.go' "$fixture/output" >/dev/null || fail 'missing gofmt diagnostic'
unset CHECK_TEST_UNFORMATTED

# Docker failures clean up only the resources that were created.
for prefix in 'docker build' 'docker network create' 'docker create' 'docker start -a'; do
  export CHECK_TEST_FAIL_PREFIX=$prefix
  run_check
  fails
  if [[ $prefix == 'docker create' || $prefix == 'docker start -a' ]]; then
    contains 'docker [pr] network rm'
  else
    absent 'docker [pr] network rm'
  fi
done
unset CHECK_TEST_FAIL_PREFIX
export CHECK_TEST_FAIL_PREFIX='docker exec'
run_check
fails
contains 'docker [pr] logs '
contains 'docker [pr] rm -f'
absent 'docker [pr] start -a'
[[ $(grep -c 'docker \[pr\] exec' "$CHECK_TEST_COMMANDS") == 60 ]] || fail 'wrong readiness retry count'
unset CHECK_TEST_FAIL_PREFIX

# Container exit codes and cleanup errors fail the check.
export CHECK_TEST_CONTAINER_EXIT=17
run_check
fails
grep -Fx 'fuzz_failed=true' "$GITHUB_OUTPUT" >/dev/null || fail 'Docker test failure did not request fuzz artifacts'
contains 'docker [pr] rm -f'
contains 'docker [pr] network rm'
unset CHECK_TEST_CONTAINER_EXIT
# Match the cleanup command using the generated ID from a mocked network create.
# The stub cannot know that ID in advance, so fail every rm through a wrapper.
mv "$fixture/bin/docker" "$fixture/bin/docker-stub"
cat > "$fixture/bin/docker" <<'SCRIPT'
#!/usr/bin/env bash
if [[ $1 == rm ]]; then exit 17; fi
exec "$(dirname "$0")/docker-stub" "$@"
SCRIPT
chmod +x "$fixture/bin/docker"
run_check
fails
rm "$fixture/bin/docker"
mv "$fixture/bin/docker-stub" "$fixture/bin/docker"

# Invalid arguments cannot skip checks or enable other modes.
for argument in --help --pr nonsense ''; do
  run_check "$argument"
  [[ $result == 2 ]] || fail 'invalid argument accepted'
  [[ ! -s $CHECK_TEST_COMMANDS ]] || fail 'invalid argument ran commands'
done
run_check --gate --gate
[[ $result == 2 ]] || fail 'multiple arguments accepted'

# Two runs share the image but never container or network names.
CHECK_TEST_COMMANDS="$fixture/first" bash "$fixture/repo/scripts/check.sh" > "$fixture/first-output" 2>&1 &
first=$!
CHECK_TEST_COMMANDS="$fixture/second" bash "$fixture/repo/scripts/check.sh" > "$fixture/second-output" 2>&1 &
second=$!
wait "$first"
wait "$second"
first_id=$(grep 'network create' "$fixture/first" | awk '{print $5}')
second_id=$(grep 'network create' "$fixture/second" | awk '{print $5}')
[[ -n $first_id && -n $second_id && $first_id != "$second_id" ]] || fail 'parallel names collide'
# Docker uses Go's CPU defaults while keeping readonly module resolution.
grep -Fx 'ENV GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS="-mod=readonly"' "$root/test/Dockerfile" >/dev/null \
  || fail 'Docker Go settings differ from the uncapped defaults'
if grep -E 'GOMAXPROCS|(^|[[:space:]"=])-p([=[:space:]]|$)' "$root/test/Dockerfile" >/dev/null; then
  fail 'Docker image caps Go parallelism'
fi

# The internal Docker step also race-tests the tagged app wiring.
: > "$CHECK_TEST_COMMANDS"
export S3_SMB_CHECK_MODE=gate S3_SMB_E2E_ENDPOINT=http://minio:9000
export S3_SMB_TEST_ARTIFACTS="$fixture/logs"
touch "$fixture/logs/daemon.log"
chmod 600 "$fixture/logs/daemon.log"
export CHECK_TEST_SOURCE="$fixture/repo"
run_internal() {
  bash -c 'cd() { builtin cd "$CHECK_TEST_SOURCE"; }; source "$1"' \
    _ "$root/test/run-linux.sh" > "$fixture/internal-output" 2>&1
}
run_internal
contains 'go [gate] build -buildvcs=false -o /tmp/s3-smb .'
contains 'go [gate] test -race -shuffle=on -count=1 -timeout=30m ./...'
contains 'go [gate] test -race -shuffle=on -count=1 -tags smbnext ./internal/app/...'
[[ $(stat -c %a "$fixture/logs/daemon.log") == 644 ]] || fail 'logs not made readable'
export CHECK_TEST_FAIL='go build -buildvcs=false -o /tmp/s3-smb .'
if run_internal; then fail 'internal build failure ignored'; fi
export CHECK_TEST_FAIL='go test -race -shuffle=on -count=1 -tags smbnext ./internal/app/...'
if run_internal; then fail 'tagged app test failure ignored'; fi
unset CHECK_TEST_FAIL
export S3_SMB_TEST_ARTIFACTS="$fixture/missing-logs"
if run_internal; then fail 'log permission failure ignored'; fi
export S3_SMB_TEST_ARTIFACTS="$fixture/logs"
for variable in S3_SMB_CHECK_MODE S3_SMB_E2E_ENDPOINT S3_SMB_TEST_ARTIFACTS; do
  if env -u "$variable" bash "$root/test/run-linux.sh" > "$fixture/internal-output" 2>&1; then
    fail "internal step accepted missing $variable"
  fi
done
# Only dispatched gates receive the longer job timeout.
grep -Fx "    timeout-minutes: \${{ inputs.gate && 180 || 60 }}" \
  "$root/.github/workflows/check.yml" >/dev/null || fail 'wrong workflow timeout'
echo 'check.sh tests passed'
