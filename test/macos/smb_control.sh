#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Independent Apple SMB control. Run only on a disposable, allocated hosted Mac.
set -euo pipefail
umask 077
[[ "$(uname -s)" = Darwin && $EUID = 0 ]]
: "${CONTROL_WORK:?}"
: "${CONTROL_ARTIFACTS:?}"
[[ "$CONTROL_WORK" = /* && "$CONTROL_ARTIFACTS" = /* ]]
[[ ! -e "$CONTROL_WORK" && ! -e "$CONTROL_ARTIFACTS" ]]
repo=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -m 700 "$CONTROL_WORK" "$CONTROL_ARTIFACTS"
# The SMB account needs traversal, not a listing, of task scratch. Files remain 0600.
chmod 711 "$CONTROL_WORK"
work=$CONTROL_WORK
out=$CONTROL_ARTIFACTS
share="$work/share"
mountpoint="$work/mount"
mkdir "$share" "$mountpoint"
private="$work/setup-private.log"
# This generated fixture credential is never written to an uploaded file.
password=$(/usr/bin/openssl rand -hex 16)
account=swarm64
capture_pid=''
mounted=0
start=$(/bin/date -u '+%Y-%m-%d %H:%M:%S')
finish() {
  result=$?
  trap - EXIT
  set +e
  if [[ $mounted = 1 ]]; then
    /sbin/umount "$mountpoint" >>"$private" 2>&1
    unmount_code=$?
    printf 'unmount_exit=%s\n' "$unmount_code" >> "$out/result.txt"
    if [[ $unmount_code != 0 ]]; then result=1; fi
  fi
  if [[ -n "$capture_pid" ]]; then
    /bin/kill -INT "$capture_pid"
    wait "$capture_pid"
    printf 'tcpdump_exit=%s\n' "$?" >> "$out/result.txt"
  fi
  if [[ -s "$work/control.pcap" ]]; then
    # Keep every byte-bearing parser output private too. Metadata only avoids
    # treating a marker scan as a proof that unparsed tails contain no secrets.
    python3 "$repo/test/macos/framing_capture.py" "$work/control.pcap" "$work/framing-private" >"$work/parser-private.log" 2>&1
    printf 'parser_exit=%s\n' "$?" >> "$out/result.txt"
    mkdir -p "$out/framing"
    for metadata in frames.jsonl capture-summary.json; do
      if [[ -f "$work/framing-private/$metadata" ]]; then
        cp "$work/framing-private/$metadata" "$out/framing/$metadata"
      fi
    done
    /usr/bin/shasum -a 256 "$work/control.pcap" | /usr/bin/awk '{print $1}' > "$out/raw-capture-sha256.txt"
    /usr/bin/stat -f '%z' "$work/control.pcap" > "$out/raw-capture-bytes.txt"
  fi
  # Do not upload arbitrary system log text or the raw authentication stream.
  /usr/bin/log show --start "$start" --style json --info --debug \
    --predicate 'senderImagePath CONTAINS "smbfs" OR process == "smbd"' > "$work/system-private.json" 2>"$work/log-private.err"
  python3 - "$work/system-private.json" > "$out/kernel-error-counts.json" <<'PY'
import collections, json, pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text(errors='replace')
terms = ['sock_receive error 54', 'TRAN_SEND returned non-fatal', 'Bad struct size',
         'IO Mismatched', 'timed out', 'reconnect', 'ENOBUFS']
print(json.dumps({term: text.count(term) for term in terms}, indent=2))
PY
  printf 'workload_exit=%s\n' "$result" >> "$out/result.txt"
  /bin/df -k > "$out/disk-after.txt"
  # Raw pcap/private logs intentionally stay in task scratch, never artifacts.
  exit "$result"
}
trap finish EXIT
{
  /usr/bin/sw_vers
  /usr/bin/uname -a
  printf 'ImageOS=%s ImageVersion=%s\n' "${ImageOS:-unknown}" "${ImageVersion:-unknown}"
  git -C "$repo" rev-parse HEAD
  /usr/sbin/sysctl kern.osrelease kern.osversion net.inet.tcp.sendspace net.inet.tcp.recvspace
  /usr/sbin/tcpdump --version
  python3 --version
} > "$out/platform.txt" 2>&1
/bin/df -k > "$out/disk-before.txt"
# Avoid a false transport result induced by filling the runner's disk.
python3 - "$work" <<'PY'
import shutil, sys
if shutil.disk_usage(sys.argv[1]).free < 75 * 2**30:
    raise RuntimeError('control needs 75 GiB free for unrotated capture')
PY
/usr/sbin/sysadminctl -addUser "$account" -password "$password" >>"$private" 2>&1
/usr/bin/pwpolicy -u "$account" -sethashtypes SMB-NT on >>"$private" 2>&1 || true
/usr/bin/dscl . -passwd "/Users/$account" "$password" >>"$private" 2>&1
/usr/sbin/chown "$account" "$share"
/usr/sbin/sharing -a "$share" -n Swarm64 -S Swarm64 -s 001 -g 000 >>"$private" 2>&1
/bin/launchctl enable system/com.apple.smbd >>"$private" 2>&1
/bin/launchctl bootstrap system /System/Library/LaunchDaemons/com.apple.smbd.plist >>"$private" 2>&1 || true
/bin/launchctl kickstart -k system/com.apple.smbd >>"$private" 2>&1 || true
printf 'rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port 1445 -> 127.0.0.1 port 445\n' > "$work/pf.conf"
/sbin/pfctl -f "$work/pf.conf" >>"$private" 2>&1
/sbin/pfctl -e >>"$private" 2>&1 || true
/sbin/pfctl -s info 2>/dev/null | /usr/bin/grep -q 'Status: Enabled'
/sbin/pfctl -s nat > "$out/pf-nat.txt" 2>&1
/usr/sbin/tcpdump -i lo0 -n -s 0 -B 4096 -U -w "$work/control.pcap" \
  'tcp and host 127.0.0.1 and (port 1445 or port 445)' 2> "$out/tcpdump.txt" &
capture_pid=$!
for attempt in $(/usr/bin/seq 1 50); do
  if /usr/bin/grep -q 'listening on' "$out/tcpdump.txt"; then break; fi
  /bin/kill -0 "$capture_pid"
  /bin/sleep .1
done
/usr/bin/grep -q 'listening on' "$out/tcpdump.txt"
/sbin/mount_smbfs -N "//$account:$password@127.0.0.1:1445/Swarm64" "$mountpoint" >>"$private" 2>&1
mounted=1
unset password
/usr/bin/smbutil statshares -a > "$out/statshares.txt" 2>&1
python3 "$repo/test/macos/smb_control_write.py" "$mountpoint/load" > "$out/write.jsonl" 2>&1
# Read the final files directly through the server's local filesystem as well.
# This guards against a client-cache-only hash check.
python3 - "$out/write.jsonl" "$share/load" > "$out/server-file-check.json" <<'PY'
import hashlib, json, pathlib, sys
lines = [json.loads(line) for line in pathlib.Path(sys.argv[1]).read_text().splitlines()]
result = next(line for line in lines if line['event'] == 'control-write-complete')
for file in result['files']:
    path = pathlib.Path(sys.argv[2]) / file['file']
    assert path.stat().st_size == file['size']
    digest = hashlib.sha256()
    with path.open('rb') as inp:
        while block := inp.read(2**20):
            digest.update(block)
    assert digest.hexdigest() == file['sha256']
print(json.dumps(dict(server_files_verified=len(result['files']))))
PY
printf 'Independent Apple smbd workload finished. Framing verdict is separate.\n'
