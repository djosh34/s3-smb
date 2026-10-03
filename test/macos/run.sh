#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
set -euo pipefail
umask 077
cd "$(dirname "$0")/../.."
repo=$PWD
: "${MAC_ARTIFACTS:?absolute evidence directory required}"
: "${MAC_WORK:?absolute task-owned work directory required}"
: "${PUBLIC_VERSION:?immutable published version required}"
: "${MAC_PHASE:?discover, backup, recover or scenario required}"
: "${MAC_TRANSFER:?absolute transfer directory required}"
# acceptance.py stops itself this many minutes after the start, so that its
# cleanup runs before the job timeout in macos.yml.
case "$MAC_PHASE" in
  discover) minutes=25 ;;
  backup) minutes=140 ;;
  recover) minutes=80 ;;
  scenario) minutes=110 ;;
  diagnostic) minutes=55 ;;
  *) echo "MAC_PHASE must be discover, backup, recover or scenario" >&2; exit 1 ;;
esac
[[ "$MAC_PHASE" != scenario || -n "${MAC_SCENARIO:-}" ]]
export MAC_DEADLINE_EPOCH=$(( $(date +%s) + minutes * 60 ))
[[ "$MAC_ARTIFACTS" = /* && "$MAC_WORK" = /* && "$MAC_WORK" != / && "$MAC_TRANSFER" = /* && "$MAC_TRANSFER" != / ]]
[[ ! -e "$MAC_WORK" && ! -e "$MAC_ARTIFACTS" ]]
if [[ "$MAC_PHASE" = recover ]]; then
  [[ -f "$MAC_TRANSFER/store.tar" && -d "$MAC_TRANSFER/reference" ]]
else
  [[ ! -e "$MAC_TRANSFER" ]]
  mkdir -m 700 "$MAC_TRANSFER"
fi
mkdir -m 700 "$MAC_WORK" "$MAC_ARTIFACTS"
export MAC_BIN="$MAC_WORK/bin"
# No native stdout/stderr (including build/help/exclusion output) reaches Actions.
# The workflow always retains these task-owned logs as authenticated ciphertext.
exec > "$MAC_ARTIFACTS/harness-private.log" 2>&1
/bin/bash "$repo/test/macos/build.sh"
# sudo is ordinary administration, NOT proof of Full Disk Access. Every native
# operation must succeed; no TCC/SIP modification is attempted.
sudo -n /usr/bin/env "PATH=$PATH" "HOME=$HOME" "MAC_WORK=$MAC_WORK" \
  "MAC_ARTIFACTS=$MAC_ARTIFACTS" "MAC_RUNNER_HOME=$HOME" "MAC_DEADLINE_EPOCH=$MAC_DEADLINE_EPOCH" \
  "MAC_BIN=$MAC_BIN" "MAC_PHASE=$MAC_PHASE" "MAC_SCENARIO=${MAC_SCENARIO:-}" "MAC_TRANSFER=$MAC_TRANSFER" \
  "MAC_IMAGE=${ImageOS:-unknown} ${ImageVersion:-unknown}" \
  PYTHONDONTWRITEBYTECODE=1 \
  python3 "$repo/test/macos/diagnostic.py"
# acceptance.py runs as root. The artifact upload runs as the runner user.
sudo -n chown -R "$(id -u):$(id -g)" "$MAC_TRANSFER"
