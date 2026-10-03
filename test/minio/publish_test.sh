#!/usr/bin/env bash
# Test publication preflight without a registry or GitHub credentials.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/repo/test" "$fixture/bin"
export MINIO_TEST_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export MINIO_TEST_PREVIOUS=$MINIO_TEST_COMMIT
export MINIO_TEST_COMMANDS="$fixture/commands"
export GITHUB_OUTPUT="$fixture/output"
cat > "$fixture/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
echo curl >> "$MINIO_TEST_COMMANDS"
if [[ ${MINIO_TEST_NETWORK_ERROR:-} == true ]]; then exit 17; fi
if [[ "$*" == *'ghcr.io/token?'* ]]; then
  printf '{"token":"test-read-token"}\n'
else
  printf '%s' "${MINIO_TEST_HTTP:-404}"
fi
STUB
cat > "$fixture/bin/git" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
echo "git $*" >> "$MINIO_TEST_COMMANDS"
if [[ ${MINIO_TEST_GIT_ERROR:-} == true ]]; then exit 17; fi
if [[ $1 == show ]]; then printf 'ARG MINIO_COMMIT=%s\n' "$MINIO_TEST_PREVIOUS"; fi
STUB
chmod +x "$fixture/bin/curl" "$fixture/bin/git"
export PATH="$fixture/bin:$PATH"
unset EVENT BEFORE MINIO_TEST_HTTP MINIO_TEST_NETWORK_ERROR MINIO_TEST_GIT_ERROR
cd "$fixture/repo"
pin() {
  printf 'ARG MINIO_RELEASE=RELEASE.2099-01-02T03-04-05Z\nARG MINIO_COMMIT=%s\n' "$MINIO_TEST_COMMIT" > test/Dockerfile
}
run() {
  : > "$GITHUB_OUTPUT"
  : > "$MINIO_TEST_COMMANDS"
  if bash "$root/test/minio/publish.sh" > "$fixture/log" 2>&1; then result=0; else result=$?; fi
}
fail() { echo "MinIO publication test failed: $*" >&2; exit 1; }
output() { grep -Fx "$1" "$GITHUB_OUTPUT" >/dev/null || fail "missing $1"; }
pin
run
[[ $result == 0 ]] || fail 'new release rejected'
output publish=true
output release=RELEASE.2099-01-02T03-04-05Z
output "commit=$MINIO_TEST_COMMIT"
export MINIO_TEST_HTTP=200
run
[[ $result == 0 ]] || fail 'existing release check failed'
output publish=false
for status in 401 403 429 500; do
  export MINIO_TEST_HTTP=$status
  run
  [[ $result != 0 ]] || fail "HTTP $status allowed publication"
  if grep -Fx publish=true "$GITHUB_OUTPUT"; then fail 'registry error allowed publication'; fi
done
unset MINIO_TEST_HTTP
export MINIO_TEST_NETWORK_ERROR=true
run
[[ $result != 0 ]] || fail 'network error allowed publication'
unset MINIO_TEST_NETWORK_ERROR
export EVENT=push BEFORE=previous
run
[[ $result == 0 ]] || fail 'unchanged pin failed'
output publish=false
if grep -Fx curl "$MINIO_TEST_COMMANDS"; then fail 'unchanged pin contacted registry'; fi
export MINIO_TEST_PREVIOUS=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
run
[[ $result == 0 ]] || fail 'changed pin failed'
output publish=true
export MINIO_TEST_GIT_ERROR=true
run
[[ $result != 0 ]] || fail 'failed pin comparison allowed publication'
unset MINIO_TEST_GIT_ERROR
export MINIO_TEST_COMMIT=invalid
pin
run
[[ $result != 0 ]] || fail 'invalid commit accepted'
export MINIO_TEST_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
pin
printf 'ARG MINIO_RELEASE=invalid\n' > test/Dockerfile
run
[[ $result != 0 ]] || fail 'invalid release accepted'
echo 'MinIO publication tests passed'
