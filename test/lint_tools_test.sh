#!/usr/bin/env bash
# Test downloads, cache reuse and failures without network access.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin"
export LINT_TEST_COMMANDS="$fixture/commands"
export LINT_TEST_SHA256
LINT_TEST_SHA256=$(command -v sha256sum)
export XDG_CACHE_HOME="$fixture/cache"
cat > "$fixture/bin/stub" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
name=${0##*/}
case "$name" in
  uname)
    if [[ $1 == -s ]]; then echo Linux; else echo "$LINT_TEST_ARCH"; fi ;;
  sha256sum)
    if [[ $1 != --check ]]; then exec "$LINT_TEST_SHA256" "$@"; fi
    while IFS= read -r line; do echo "$line" >> "$LINT_TEST_COMMANDS"; done
    [[ ${LINT_TEST_FAIL:-} != checksum ]] ;;
  curl)
    printf '%s\n' "$*" >> "$LINT_TEST_COMMANDS"
    [[ ${LINT_TEST_FAIL:-} != download ]] || exit 1
    printf archive > "${@: -1}" ;;
  tar)
    [[ ${LINT_TEST_FAIL:-} != extract ]] || exit 1
    directory=$4 member=$5
    mkdir -p "$directory/$(dirname "$member")"
    printf '#!/usr/bin/env bash\nexit 0\n' > "$directory/$member" ;;
esac
STUB
chmod +x "$fixture/bin/stub"
for name in uname sha256sum curl tar; do ln -s stub "$fixture/bin/$name"; done
export PATH="$fixture/bin:$PATH"
fail() { echo "Lint tools test failed: $*" >&2; exit 1; }
for arch in aarch64 x86_64; do
  export LINT_TEST_ARCH=$arch
  : > "$LINT_TEST_COMMANDS"
  tools=$(bash "$root/scripts/lint-tools.sh")
  for name in golangci-lint actionlint shellcheck; do
    [[ -x $tools/$name ]] || fail "missing $name for $arch"
  done
  [[ $(grep -c 'https://' "$LINT_TEST_COMMANDS") == 3 ]] || fail 'wrong download count'
  grep -F '/v2.14.0/golangci-lint-2.14.0-linux-' "$LINT_TEST_COMMANDS" >/dev/null || fail 'wrong golangci pin'
  grep -F '/v1.7.12/actionlint_1.7.12_linux_' "$LINT_TEST_COMMANDS" >/dev/null || fail 'wrong actionlint pin'
  grep -F "/v0.11.0/shellcheck-v0.11.0.linux.$arch.tar.gz" "$LINT_TEST_COMMANDS" >/dev/null || fail 'wrong shellcheck pin'
  [[ $(grep -cE '^[a-f0-9]{64}  ' "$LINT_TEST_COMMANDS") == 3 ]] || fail 'missing checksum checks'
  : > "$LINT_TEST_COMMANDS"
  [[ $(bash "$root/scripts/lint-tools.sh") == "$tools" ]] || fail 'cache path changed'
  [[ ! -s $LINT_TEST_COMMANDS ]] || fail 'cached tools downloaded again'
done
export LINT_TEST_ARCH=aarch64
for failure in download checksum extract; do
  rm -rf "$XDG_CACHE_HOME"
  export LINT_TEST_FAIL=$failure
  if bash "$root/scripts/lint-tools.sh" > "$fixture/output" 2>&1; then
    fail "ignored $failure failure"
  fi
  if find "$XDG_CACHE_HOME" -type f -print -quit | grep -q .; then
    fail "left partial files after $failure failure"
  fi
done
unset LINT_TEST_FAIL
export LINT_TEST_ARCH=unsupported
if bash "$root/scripts/lint-tools.sh" > "$fixture/output" 2>&1; then
  fail 'unsupported CPU accepted'
fi
echo 'Lint tools tests passed'
