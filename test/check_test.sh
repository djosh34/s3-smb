#!/usr/bin/env bash
# Test scripts/check.sh and test/run-linux.sh with stub commands, without
# building Go packages or starting Docker.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/repo/scripts" "$fixture/repo/test/minio" "$fixture/bin" "$fixture/logs" \
  "$fixture/repo/.github/workflows"
mkdir -p "$fixture/repo/.git"
touch "$fixture/repo/.git/ignored.go"
touch "$fixture/repo/our file.go"
cp "$root/scripts/check.sh" "$fixture/repo/scripts/check.sh"
printf 'FROM scratch\n' > "$fixture/repo/test/Dockerfile"
for script in test/check_test.sh test/minio/publish_test.sh test/lint_tools_test.sh test/lint_config_test.sh; do
  echo "echo ${script##*/} >> \"\$CHECK_TEST_COMMANDS\"" > "$fixture/repo/$script"
done
cat > "$fixture/repo/scripts/lint-tools.sh" <<'SCRIPT'
printf '%s\n' "$CHECK_TEST_TOOLS"
SCRIPT
touch "$fixture/repo/.github/workflows/check.yml"
export CHECK_TEST_TOOLS="$fixture/bin"

# Every stubbed command logs itself with the check mode. CHECK_TEST_FAIL makes
# one command fail, CHECK_TEST_FAIL_PREFIX every command with that prefix.
cat > "$fixture/bin/stub" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
command=${0##*/}
printf '%s [%s] %s\n' "$command" "$S3_SMB_CHECK_MODE" "$*" >> "$CHECK_TEST_COMMANDS"
if [[ ${GOOS:-} == darwin ]]; then
  printf 'darwin %s %s\n' "$command" "$*" >> "$CHECK_TEST_COMMANDS"
fi
if [[ -n ${S3_SMB_ROUND:-} ]]; then printf 'round %s\n' "$S3_SMB_ROUND" >> "$CHECK_TEST_COMMANDS"; fi
if [[ ${CHECK_TEST_FAIL:-} == "$command $*" ]]; then exit 17; fi
if [[ -n ${CHECK_TEST_FAIL_PREFIX:-} && "$command $*" == "$CHECK_TEST_FAIL_PREFIX"* ]]; then exit 17; fi
case "$command $*" in
  'go test -race -shuffle=on -count=1 -timeout='*' ./internal/engine ./test/e2e')
    [[ ${S3_SMB_E2E_BINARY:-} == /tmp/s3-smb && ${GORACE:-} == halt_on_error=1 && ${S3_SMB_CHAOS_SEED:-} =~ ^[0-9]+$ ]]
    printf 'chaos seed %s\n' "$S3_SMB_CHAOS_SEED" >> "$CHECK_TEST_COMMANDS" ;;
  'go list '*) printf 'example/one\n\nexample/two\n' ;;
  'go test -list ^TestChaos ./test/e2e') printf 'TestChaosX\nok example/e2e 0.01s\n' ;;
  'go test -count=1 -run ^TestBreak -v ./test/e2e')
    [[ ${S3_SMB_LIST_ROUNDS:-} == 1 ]]
    if [[ $S3_SMB_CHECK_MODE == gate ]]; then
      printf '=== RUN   TestBreakA\nrounds TestBreakA 3\nrounds TestBreakB 1\n'
    else
      printf 'rounds TestBreakA 2\nrounds TestBreakB 0\n--- SKIP: TestBreakB\n'
    fi ;;
  "go test -list ^Fuzz example/one")
    if [[ ${CHECK_TEST_TARGETS:-yes} == yes ]]; then printf 'FuzzFirst\nFuzzSecond\n'; fi
    printf 'ok example/one 0.01s\n' ;;
  "go test -list ^Fuzz example/two")
    if [[ ${CHECK_TEST_TARGETS:-yes} == yes ]]; then printf 'FuzzOther\n'; fi
    printf '? example/two [no test files]\n' ;;
  'gofmt '*) printf '%s' "${CHECK_TEST_UNFORMATTED:-}" ;;
  'docker image inspect '*) exit 1 ;;
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
unset CHECK_TEST_FAIL CHECK_TEST_FAIL_PREFIX CHECK_TEST_TARGETS CHECK_TEST_UNFORMATTED CHECK_TEST_CONTAINER_EXIT

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

# PR mode runs every static check, the unit tests and the Docker integration.
export S3_SMB_CHECK_MODE=gate
run_check
succeeds
contains 'golangci-lint [pr] config verify'
contains 'golangci-lint [pr] run ./...'
contains 'darwin golangci-lint run --build-tags macos ./test/macos/...'
contains 'shellcheck [pr] '
contains "actionlint [pr] -shellcheck $fixture/bin/shellcheck -pyflakes "
contains 'go [pr] mod tidy -diff'
contains 'go [pr] vet ./...'
contains 'darwin go vet -tags macos ./test/macos/...'
contains 'gofmt [pr] -l ./our file.go'
absent 'ignored.go'
for script in check_test.sh lint_tools_test.sh lint_config_test.sh publish_test.sh; do
  contains "$script"
done
contains 'go [pr] test -race -shuffle=on -count=1 ./...'
absent '-fuzz '
contains 'docker [pr] build -f test/Dockerfile -t s3-smb-test:'
contains '-e S3_SMB_CHECK_MODE=pr -e S3_SMB_CHAOS_SEED '
contains 'bash /src/test/run-linux.sh'
contains 'docker [pr] rm -f'
contains 'docker [pr] network rm'

# Gate mode also fuzzes every target in its own package.
run_check --gate
succeeds
contains 'go [gate] test -run ^$ -fuzz ^FuzzFirst$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^FuzzSecond$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^FuzzOther$ -fuzztime 1m -parallel 2 example/two'
contains '-e S3_SMB_CHECK_MODE=gate'
export CHECK_TEST_TARGETS=no
run_check --gate
succeeds
absent ' -fuzz '
unset CHECK_TEST_TARGETS

# Each part runs alone. A fuzz shard takes every SHARDS-th target from SHARD on.
run_check lint
succeeds
contains 'golangci-lint [pr] run ./...'
absent 'go [pr] test '
absent 'docker '
run_check unit
succeeds
contains 'go [pr] test -race -shuffle=on -count=1 ./...'
absent 'golangci-lint'
absent 'docker '
run_check --gate integration
succeeds
contains '-e S3_SMB_CHECK_MODE=gate'
absent 'golangci-lint'
absent 'go [gate] test '
run_check fuzz
succeeds
contains 'go [pr] test -run ^$ -fuzz ^FuzzFirst$'
contains 'go [pr] test -run ^$ -fuzz ^FuzzOther$'
absent 'golangci-lint'
absent 'docker '
run_check --gate fuzz 2/2
succeeds
contains 'go [gate] test -run ^$ -fuzz ^FuzzSecond$'
absent '^FuzzFirst$'
absent '^FuzzOther$'
run_check fuzz 1/2
succeeds
contains '^FuzzFirst$'
contains '^FuzzOther$'
absent '^FuzzSecond$'
run_check --gate integration TestBreakA-2
succeeds
contains '-e S3_SMB_JOB=TestBreakA-2'
run_check integration
succeeds
contains '-e S3_SMB_JOB=all'
# jobs lists each chaos and break test, or in the gate each break test round.
run_check jobs
succeeds
[[ $(cat "$fixture/output") == '["TestChaosX","TestBreakA","rest"]' ]] || fail "PR jobs: $(cat "$fixture/output")"
run_check --gate jobs
succeeds
[[ $(cat "$fixture/output") == '["TestChaosX","TestBreakA-0","TestBreakA-1","TestBreakA-2","TestBreakB-0","rest"]' ]] ||
  fail "gate jobs: $(cat "$fixture/output")"

# A failure stops later stages. Test and fuzz failures request fuzz artifacts.
for command in 'golangci-lint run ./...' \
  'golangci-lint run --build-tags macos ./test/macos/...' 'go mod tidy -diff' \
  'go vet ./...' 'go vet -tags macos ./test/macos/...' \
  'go test -race -shuffle=on -count=1 ./...' \
  'go test -run ^$ -fuzz ^FuzzOther$ -fuzztime 1m -parallel 2 example/two'; do
  export CHECK_TEST_FAIL=$command
  run_check --gate
  fails
  absent 'docker [gate] network create'
  case "$command" in
    'go test '*)
      grep -Fx 'fuzz_failed=true' "$GITHUB_OUTPUT" >/dev/null || fail 'missing fuzz artifact signal' ;;
  esac
done
unset CHECK_TEST_FAIL
for prefix in 'shellcheck ' 'actionlint '; do
  export CHECK_TEST_FAIL_PREFIX=$prefix
  run_check
  fails
  absent 'go [pr] mod tidy'
done
unset CHECK_TEST_FAIL_PREFIX
export CHECK_TEST_UNFORMATTED=$'./bad.go\n'
run_check
fails
absent 'go [pr] test '
grep -F './bad.go' "$fixture/output" >/dev/null || fail 'missing gofmt diagnostic'
unset CHECK_TEST_UNFORMATTED

# Docker failures fail the check and remove only what was created.
for prefix in 'docker build' 'docker network create' 'docker create' 'docker exec' 'docker start -a'; do
  export CHECK_TEST_FAIL_PREFIX=$prefix
  run_check
  fails
  if [[ $prefix == 'docker build' || $prefix == 'docker network create' ]]; then
    absent 'docker [pr] network rm'
  else
    contains 'docker [pr] network rm'
  fi
done
unset CHECK_TEST_FAIL_PREFIX
export CHECK_TEST_CONTAINER_EXIT=17
run_check
fails
contains 'docker [pr] rm -f'
unset CHECK_TEST_CONTAINER_EXIT

for arguments in --help --pr nonsense '' '--gate --gate' 'lint unit' 'unit 1/2' 'fuzz 0/2' 'fuzz 3/2' \
  'fuzz 1/2 lint' 'fuzz 1-2' 'lint --gate' 'integration 0/2' 'integration 3/2' 'integration TestBreakA-x' \
  'integration bogus' 'jobs 1'; do
  if [[ -z $arguments ]]; then run_check ''; else read -ra words <<< "$arguments"; run_check "${words[@]}"; fi
  [[ $result == 2 ]] || fail "arguments $arguments accepted"
  [[ ! -s $CHECK_TEST_COMMANDS ]] || fail 'invalid arguments ran commands'
done

# run-linux.sh runs the MinIO, integration, Samba and chaos tests against a
# race build and leaves the logs readable. The chaos tests get a new seed
# unless one is given.
export S3_SMB_E2E_ENDPOINT=http://minio:9000 S3_SMB_TEST_ARTIFACTS="$fixture/logs"
touch "$fixture/logs/daemon.log"
chmod 600 "$fixture/logs/daemon.log"
export CHECK_TEST_SOURCE="$fixture/repo"
run_internal() {
  : > "$CHECK_TEST_COMMANDS"
  bash -c 'cd() { builtin cd "$CHECK_TEST_SOURCE"; }; source "$1"' \
    _ "$root/test/run-linux.sh" > "$fixture/internal-output" 2>&1
}
run_internal || fail 'run-linux.sh failed'
contains 'go [gate] build -race -buildvcs=false -o /tmp/s3-smb .'
contains 'go [gate] test -race -shuffle=on -count=1 -timeout=120m ./internal/engine ./test/e2e'
[[ $(stat -c %a "$fixture/logs/daemon.log") == 644 ]] || fail 'logs not made readable'
contains 'chaos seed '
S3_SMB_CHECK_MODE='pr' run_internal || fail 'run-linux.sh failed in PR mode'
contains 'go [pr] test -race -shuffle=on -count=1 -timeout=60m ./internal/engine ./test/e2e'
S3_SMB_CHAOS_SEED=42 run_internal || fail 'run-linux.sh failed with a chaos seed'
contains 'chaos seed 42'
grep -F 'replay with S3_SMB_CHAOS_SEED=42' "$fixture/internal-output" >/dev/null || fail 'chaos seed not printed'
# A job runs every test but the chaos and break tests, one test, or one round
# of a break test.
S3_SMB_JOB=rest run_internal || fail 'run-linux.sh failed as job rest'
contains "-skip ^(TestBreak|TestChaos) ./internal/engine ./test/e2e"
S3_SMB_JOB=TestChaosX run_internal || fail 'run-linux.sh failed as job TestChaosX'
contains '-run ^TestChaosX$ ./test/e2e'
absent 'round '
S3_SMB_JOB=TestBreakA-2 run_internal || fail 'run-linux.sh failed as job TestBreakA-2'
contains '-run ^TestBreakA$ ./test/e2e'
contains 'round 2'
if S3_SMB_JOB=bogus run_internal; then fail 'run-linux.sh accepted an unknown job'; fi
for command in 'go build -race -buildvcs=false -o /tmp/s3-smb .' \
  'go test -race -shuffle=on -count=1 -timeout=120m ./internal/engine ./test/e2e'; do
  export CHECK_TEST_FAIL=$command
  if run_internal; then fail "run-linux.sh ignored a failure: $command"; fi
done
unset CHECK_TEST_FAIL
for variable in S3_SMB_CHECK_MODE S3_SMB_E2E_ENDPOINT S3_SMB_TEST_ARTIFACTS; do
  if env -u "$variable" bash "$root/test/run-linux.sh" > "$fixture/internal-output" 2>&1; then
    fail "run-linux.sh accepted missing $variable"
  fi
done
echo 'check.sh tests passed'
