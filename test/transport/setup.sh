#!/usr/bin/env bash
# Invoked only inside the shared disposable Linux test container, never on host.
set -Eeuo pipefail
[[ -f /.dockerenv ]] || { echo 'transport setup requires the disposable Docker runner' >&2; exit 1; }
# MinIO fixture must set MINIO_DOMAIN=minio,transport.minio. The proxy keeps the
# native client's signed Host intact; these names address its local TLS listener.
if ! grep -q '127.0.0.1 transport.minio transport-test.transport.minio' /etc/hosts; then
  printf '\n127.0.0.1 transport.minio transport-test.transport.minio\n' >>/etc/hosts
fi
# Do not allow a developer/CI HTTP proxy to bypass this isolated fixture.
export NO_PROXY="${NO_PROXY:+$NO_PROXY,}transport.minio,.transport.minio,minio"
export no_proxy="$NO_PROXY"
export S3_SMB_TRANSPORT=1
if (( $# )); then exec "$@"; fi
