#!/usr/bin/env bash
# Exercise orchestration without building Go packages or starting Docker.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/repo/scripts" "$fixture/repo/test" "$fixture/bin" "$fixture/logs"
cp "$root/scripts/check.sh" "$fixture/repo/scripts/check.sh"
printf 'FROM scratch\n' > "$fixture/repo/test/Dockerfile"
printf 'echo script-tests >> "$CHECK_TEST_COMMANDS"\n' > "$fixture/repo/test/check_test.sh"

cat > "$fixture/bin/stub" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
command=${0##*/}
command=${command%-stub}
printf '%s [%s] %s\n' "$command" "$S3_SMB_CHECK_MODE" "$*" >> "$CHECK_TEST_COMMANDS"
if [[ ${CHECK_TEST_FAIL:-} == "$command $*" ]]; then exit 17; fi
case "$command $*" in
  'go list '*) printf 'example/one\n\nexample/two\n' ;;
  "go test -list ^Fuzz example/one")
    if [[ ${CHECK_TEST_TARGETS:-yes} == yes ]]; then printf 'FuzzFirst\nFuzzSecond\n'; fi
    printf 'ok example/one 0.01s\n' ;;
  "go test -list ^Fuzz example/two")
    if [[ ${CHECK_TEST_TARGETS:-yes} == yes ]]; then printf 'FuzzOther\n'; fi
    printf '? example/two [no test files]\n' ;;
  'gofmt '*) printf '%s' "${CHECK_TEST_UNFORMATTED:-}" ;;
  'docker image inspect '*) [[ ${CHECK_TEST_CACHED:-no} == yes ]] ;;
  'docker inspect '*) printf '%s\n' "${CHECK_TEST_CONTAINER_EXIT:-0}" ;;
esac
STUB
chmod +x "$fixture/bin/stub"
for command in go gofmt docker sleep; do ln -s stub "$fixture/bin/$command"; done
export PATH="$fixture/bin:$PATH"
export CHECK_TEST_COMMANDS="$fixture/commands"
export S3_SMB_TEST_LOGS="$fixture/logs"
export GITHUB_OUTPUT="$fixture/github-output"
unset CHECK_TEST_FAIL CHECK_TEST_CACHED CHECK_TEST_TARGETS CHECK_TEST_UNFORMATTED CHECK_TEST_CONTAINER_EXIT

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
contains 'go [pr] mod tidy -diff'
contains 'go [pr] vet ./...'
contains 'go [pr] vet -tags smbnext ./...'
contains 'gofmt [pr] -l'
contains 'script-tests'
contains 'go [pr] test -count=1 ./...'
contains 'go [pr] test -race -shuffle=on -count=1 ./...'
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
contains 'go [gate] test -count=1 ./...'
contains 'go [gate] test -run ^$ -fuzz ^FuzzFirst$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^FuzzSecond$ -fuzztime 1m -parallel 2 example/one'
contains 'go [gate] test -run ^$ -fuzz ^FuzzOther$ -fuzztime 1m -parallel 2 example/two'
contains '-e S3_SMB_CHECK_MODE=gate'
[[ $(grep -c -- ' -fuzz ' "$CHECK_TEST_COMMANDS") == 3 ]] || fail 'wrong fuzz target count'
export CHECK_TEST_TARGETS=no
run_check --gate
succeeds
absent ' -fuzz '
contains 'docker [gate] start -a'
unset CHECK_TEST_TARGETS

# Failures stop later stages. Seed and exploration failures request artifacts.
for command in 'go mod tidy -diff' 'go vet ./...' 'go vet -tags smbnext ./...' \
  'go test -count=1 ./...' 'go test -race -shuffle=on -count=1 ./...' \
  'go list -f {{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}} ./...' \
  'go test -list ^Fuzz example/one' \
  'go test -run ^$ -fuzz ^FuzzFirst$ -fuzztime 1m -parallel 2 example/one'; do
  export CHECK_TEST_FAIL=$command
  run_check --gate
  fails
  absent 'docker [gate] network create'
  case "$command" in
    'go test -count=1 ./...'|'go test -race '*|'go test -run '*)
      grep -Fx 'fuzz_failed=true' "$GITHUB_OUTPUT" >/dev/null || fail 'missing fuzz artifact signal' ;;
  esac
done
unset CHECK_TEST_FAIL
export CHECK_TEST_UNFORMATTED=$'./bad.go\n'
run_check
fails
absent 'go [pr] test '
grep -F './bad.go' "$fixture/output" >/dev/null || fail 'missing gofmt diagnostic'
unset CHECK_TEST_UNFORMATTED

# Container exit codes and cleanup errors fail the check.
export CHECK_TEST_CONTAINER_EXIT=17
run_check
fails
contains 'docker [pr] rm -f'
contains 'docker [pr] network rm'
unset CHECK_TEST_CONTAINER_EXIT
# Match the cleanup command using the generated ID from a mocked network create.
# The stub cannot know that ID in advance, so fail every rm through a wrapper.
mv "$fixture/bin/docker" "$fixture/bin/docker-stub"
printf '#!/usr/bin/env bash\nif [[ $1 == rm ]]; then exit 17; fi\nexec "$(dirname "$0")/docker-stub" "$@"\n' > "$fixture/bin/docker"
chmod +x "$fixture/bin/docker"
run_check
fails
rm "$fixture/bin/docker"
mv "$fixture/bin/docker-stub" "$fixture/bin/docker"

# Invalid arguments cannot skip checks or enable other modes.
for argument in --help --pr nonsense; do
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
echo 'check.sh tests passed'
