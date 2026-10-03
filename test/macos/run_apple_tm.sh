#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
set -euo pipefail
umask 077
cd "$(dirname "$0")/../.."
repo=$PWD
: "${MAC_WORK:?}"
: "${MAC_ARTIFACTS:?}"
: "${APPLE_PUBLIC:?}"
[[ "$MAC_WORK" = /* && "$MAC_ARTIFACTS" = "$MAC_WORK/private" && "$APPLE_PUBLIC" = /* ]]
[[ ! -e "$MAC_WORK" && ! -e "$APPLE_PUBLIC" ]]
mkdir -m 700 "$MAC_WORK" "$MAC_ARTIFACTS" "$APPLE_PUBLIC" "$MAC_WORK/transfer" "$MAC_WORK/bin"
# Full native/build/exception output remains private, including early failures.
exec >"$MAC_ARTIFACTS/operator.log" 2>&1
export MAC_BIN="$MAC_WORK/bin" MAC_TRANSFER="$MAC_WORK/transfer"
export MAC_PHASE=apple-control MAC_RUNNER_HOME="$HOME"
export PYTHONDONTWRITEBYTECODE=1
status=1
if cc -O2 -Wall -Wextra -Werror test/macos/passive_capture.c -lpcap -o "$MAC_WORK/passive-capture"; then
  set +e
  sudo -n /usr/bin/env "PATH=$PATH" "HOME=$HOME" "LC_ALL=C" "TERM=dumb" \
    "MAC_WORK=$MAC_WORK" "MAC_ARTIFACTS=$MAC_ARTIFACTS" "APPLE_PUBLIC=$APPLE_PUBLIC" \
    "MAC_BIN=$MAC_BIN" "MAC_TRANSFER=$MAC_TRANSFER" "MAC_RUNNER_HOME=$MAC_RUNNER_HOME" \
    "MAC_PHASE=$MAC_PHASE" "APPLE_HARNESS_SHA=$(git rev-parse HEAD)" \
    "APPLE_RUNNER_IMAGE_VERSION=${ImageVersion:-}" PYTHONDONTWRITEBYTECODE=1 \
    python3 "$repo/test/macos/apple_tm.py"
  status=$?
  set -e
fi
# Readers/services have stopped; this shell's own log is closed before archiving.
# Logs-only encryption is supported for an early capability/build failure.
exec >/dev/null 2>&1
set +e
sudo -n /usr/bin/env "PATH=$PATH" "MAC_WORK=$MAC_WORK" "MAC_ARTIFACTS=$MAC_ARTIFACTS" \
  python3 - <<'PY'
import os, pathlib, shutil
w, logs = pathlib.Path(os.environ['MAC_WORK']), pathlib.Path(os.environ['MAC_ARTIFACTS'])
raw = w / 'capture.pcap'
raw_bytes = raw.stat().st_size if raw.exists() else 0
log_bytes = sum(p.stat().st_size for p in logs.rglob('*') if p.is_file())
# Compression is not assumed; preserve20GiB beyond peak ciphertext allocation.
assert raw_bytes <= 24 * 2**30 and log_bytes <= 2**30
assert shutil.disk_usage(w).free >= raw_bytes + log_bytes + 21 * 2**30
PY
budget=$?
retention=1
openssl_path="$(brew --prefix openssl@3)/bin/openssl"
if [[ $budget -eq 0 && -x "$openssl_path" ]]; then
  sudo -n /usr/bin/env "PATH=$PATH" "CAPTURE_OPENSSL=$openssl_path" bash test/macos/encrypt_capture.sh \
    "$MAC_WORK/capture.pcap" "$APPLE_PUBLIC" test/macos/capture-recipient.pem "$MAC_ARTIFACTS" \
    >/dev/null 2>&1
  retention=$?
fi
sudo -n python3 - "$APPLE_PUBLIC" "$status" "$retention" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
trial = p / 'trial.json'
data = json.loads(trial.read_text()) if trial.exists() else dict(capability_stage='not-run',run_stage='not-run')
data.update(retention_stage='pass' if sys.argv[3]=='0' else 'fail', native_exit=int(sys.argv[2]))
trial.write_text(json.dumps(data, indent=2)+'\n')
PY
sudo -n chown -R "$(id -u):$(id -g)" "$APPLE_PUBLIC"
[[ $status -eq 0 && $retention -eq 0 ]]
