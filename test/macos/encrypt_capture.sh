#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Only the public recipient certificate is present on the runner.
set -euo pipefail
umask 077
raw=$1
public=$2
recipient=$3
private_logs=$4
[[ -d "$(dirname "$raw")" && -d "$public" && -f "$recipient" && -d "$private_logs" ]]
# Hosted Intel Mac has Homebrew OpenSSL3; no LibreSSL cipher fallback.
openssl=${CAPTURE_OPENSSL:-$(brew --prefix openssl@3)/bin/openssl}
"$openssl" version | grep '^OpenSSL 3\.' > "$public/capture-crypto-version.txt"
# CMS AuthEnvelopedData (AES-GCM), streaming BER encoding. Compress before
# encryption; shell pipefail rejects either failed stage. No plaintext fallback.
partial="$raw.encrypted.partial"
trap 'rm -f "$partial"' EXIT
archive() {
  if [[ -f "$raw" ]]; then
    tar -cf - -C "$(dirname "$raw")" "$(basename "$raw")" \
      -C "$(dirname "$private_logs")" "$(basename "$private_logs")"
  else
    # A setup/capture-readiness failure still has useful private logs. Do not
    # manufacture a pcap or call a logs-only archive a retained capture.
    tar -cf - -C "$(dirname "$private_logs")" "$(basename "$private_logs")"
  fi
}
archive | gzip -1c | \
  "$openssl" cms -encrypt -aes-256-gcm -binary -stream -outform DER -out "$partial" "$recipient"
mv "$partial" "$public/capture.tar.gz.cms"
python3 - "$raw" "$public" "$recipient" <<'PY'
import datetime, hashlib, json, pathlib, sys
raw, public, recipient = map(pathlib.Path, sys.argv[1:])
def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()
ciphertext = public / 'capture.tar.gz.cms'
(public / 'capture-encryption.json').write_text(json.dumps(dict(
    time=datetime.datetime.now(datetime.timezone.utc).isoformat(),
    algorithm='OpenSSL CMS AuthEnvelopedData AES-256-GCM; tar+gzip before encryption',
    raw_capture_present=raw.is_file(), raw_bytes=raw.stat().st_size if raw.is_file() else None,
    ciphertext_bytes=ciphertext.stat().st_size,
    ciphertext_sha256=digest(ciphertext), recipient_certificate_sha256=digest(recipient),
    plaintext_uploaded=False, private_key_on_runner=False), indent=2) + '\n')
PY
