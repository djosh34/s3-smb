#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
set -euo pipefail
umask 077
export MAC_DEADLINE_EPOCH=$(( $(date +%s) + 330 * 60 ))
cd "$(dirname "$0")/../.."
repo=$PWD
: "${MAC_ARTIFACTS:?absolute evidence directory required}"
: "${MAC_WORK:?absolute task-owned work directory required}"
: "${PUBLIC_VERSION:?immutable published version required}"
[[ "$MAC_ARTIFACTS" = /* && "$MAC_WORK" = /* && "$MAC_WORK" != / ]]
[[ ! -e "$MAC_WORK" && ! -e "$MAC_ARTIFACTS" ]]
mkdir -m 700 "$MAC_WORK" "$MAC_ARTIFACTS"
exec 3>&1 4>&2
exec > "$MAC_ARTIFACTS/entrypoint.log" 2>&1
record_exit() {
  local rc=$?
  printf '%s\n' "$rc" > "$MAC_ARTIFACTS/exit-status"
  if /bin/df -k > "$MAC_ARTIFACTS/exit-capacity.log" 2>&1; then
    printf '0\n' > "$MAC_ARTIFACTS/exit-capacity-status"
  else
    printf '%s\n' "$?" > "$MAC_ARTIFACTS/exit-capacity-status"
  fi
  # No evidence writers remain after build supervision/native cleanup. Close
  # this log too before hashing it. Handoff errors remain in the Actions log.
  exec 1>&3 2>&4
  if ! sudo -n "$(command -v python3)" "$repo/test/macos/artifacts.py" handoff \
       "$MAC_ARTIFACTS" "$(id -u)" "$(id -g)"; then
    rc=1
    printf '1\n' > "$MAC_ARTIFACTS/exit-status"
  fi
  exit "$rc"
}
trap record_exit EXIT
python3 "$repo/test/macos/deadline.py" "$MAC_DEADLINE_EPOCH" \
  "$MAC_ARTIFACTS/build-deadline.json" /bin/bash "$repo/test/macos/build.sh"
IFS= read -r MAC_BIN < "$MAC_ARTIFACTS/native-build-root"
# sudo is ordinary administration, NOT proof of Full Disk Access. Every native
# operation must succeed; no TCC/SIP modification is attempted.
sudo -n /usr/bin/env "PATH=$PATH" "HOME=$HOME" "MAC_WORK=$MAC_WORK" \
  "MAC_ARTIFACTS=$MAC_ARTIFACTS" "MAC_RUNNER_HOME=$HOME" "MAC_DEADLINE_EPOCH=$MAC_DEADLINE_EPOCH" \
  "MAC_BIN=$MAC_BIN" PYTHONDONTWRITEBYTECODE=1 \
  python3 "$repo/test/macos/acceptance.py"
