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
: "${MAC_PHASE:?backup or recover required}"
: "${MAC_SCENARIO:?password-control or named-empty label required}"
: "${MAC_TRANSFER:?absolute transfer directory required}"
[[ "$MAC_PHASE" = backup || "$MAC_PHASE" = recover ]]
[[ "$MAC_SCENARIO" = password-control || "$MAC_SCENARIO" = named-empty ]]
[[ "$MAC_ARTIFACTS" = /* && "$MAC_WORK" = /* && "$MAC_WORK" != / && "$MAC_TRANSFER" = /* && "$MAC_TRANSFER" != / ]]
[[ ! -e "$MAC_WORK" && ! -e "$MAC_ARTIFACTS" ]]
if [[ "$MAC_PHASE" = backup ]]; then
  [[ ! -e "$MAC_TRANSFER" ]]
  mkdir -m 700 "$MAC_TRANSFER"
else
  [[ -d "$MAC_TRANSFER/store" && -d "$MAC_TRANSFER/reference" ]]
fi
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
printf '%s\n' "$MAC_SCENARIO" > "$MAC_ARTIFACTS/scenario"
# Exclude Actions' transient control/credential files, never the workspace as
# a whole: it contains ordinary source which remains in the full-Mac backup.
# The hosted agent installation is CI infrastructure, not user source/content.
if [[ "$MAC_PHASE" = backup ]]; then
  : "${RUNNER_WORKSPACE:?hosted runner workspace required}"
  : "${RUNNER_TEMP:?hosted runner temporary directory required}"
  runner_root=$(dirname "$RUNNER_WORKSPACE")
  # Find the running agent's actual installation, not an assumed HOME/runners.
  # lsof emits executable paths only (-d txt), never argv or credential contents.
  runner_binary=$(sudo -n /usr/sbin/lsof -a -c Runner.Worker -d txt -Fn |
    awk '/^n.*\/bin\/Runner[.]Worker$/ {sub(/^n/, ""); binary=$0} END {print binary}')
  [[ -n "$runner_binary" ]]
  runner_install=$(dirname "$(dirname "$runner_binary")")
  [[ "$runner_install" = /* && "$runner_install" != / && "$runner_install" != "$HOME" && "$runner_install" != "$runner_root" ]]
  printf '%s\n' "$runner_install" > "$MAC_ARTIFACTS/runner-control-root.txt"
  for path in "$RUNNER_TEMP" "$runner_install" \
    "$runner_root/_actions" "$runner_root/_diag" \
    "$runner_root/.credentials" "$runner_root/.credentials_rsaparams" \
    "$runner_root/.runner" "$runner_root/.env" "$runner_root/.path"; do
    if [[ -e "$path" ]]; then
      sudo -n /usr/bin/tmutil addexclusion -p "$path"
      printf '%s\n' "$path" >> "$MAC_ARTIFACTS/ci-infrastructure-exclusions.txt"
    fi
  done
fi
printf 'native-build-start scenario=%s\n' "$MAC_SCENARIO" >&3
python3 "$repo/test/macos/deadline.py" "$MAC_DEADLINE_EPOCH" \
  "$MAC_ARTIFACTS/build-deadline.json" /bin/bash "$repo/test/macos/build.sh"
printf 'native-build-complete scenario=%s\n' "$MAC_SCENARIO" >&3
IFS= read -r MAC_BIN < "$MAC_ARTIFACTS/native-build-root"
# sudo is ordinary administration, NOT proof of Full Disk Access. Every native
# operation must succeed; no TCC/SIP modification is attempted.
sudo -n /usr/bin/env "PATH=$PATH" "HOME=$HOME" "MAC_WORK=$MAC_WORK" \
  "MAC_ARTIFACTS=$MAC_ARTIFACTS" "MAC_RUNNER_HOME=$HOME" "MAC_DEADLINE_EPOCH=$MAC_DEADLINE_EPOCH" \
  "MAC_BIN=$MAC_BIN" "MAC_PHASE=$MAC_PHASE" "MAC_TRANSFER=$MAC_TRANSFER" \
  "MAC_SCENARIO=$MAC_SCENARIO" \
  PYTHONDONTWRITEBYTECODE=1 \
  python3 "$repo/test/macos/acceptance.py" >&3
if [[ "$MAC_PHASE" = backup ]]; then
  # Only the dedicated store/reference transfer tree, never daemon-local state.
  sudo -n "$(command -v python3)" "$repo/test/macos/artifacts.py" handoff \
    "$MAC_TRANSFER" "$(id -u)" "$(id -g)"
fi
