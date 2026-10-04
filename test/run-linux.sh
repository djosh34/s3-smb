#!/usr/bin/env bash
# Internal Docker step of scripts/check.sh.
set -Eeuo pipefail
: "${S3_SMB_CHECK_MODE:?Run scripts/check.sh}"
: "${S3_SMB_E2E_ENDPOINT:?Run scripts/check.sh}"
: "${S3_SMB_TEST_ARTIFACTS:?Run scripts/check.sh}"
case "$S3_SMB_CHECK_MODE" in
  pr) chaos_timeout=15m ;;
  gate) chaos_timeout=90m ;;
  *) echo 'S3_SMB_CHECK_MODE must be pr or gate' >&2; exit 2 ;;
esac
# The tests run as root. Let the calling user read the daemon logs.
readable_logs() {
  status=$?
  if ! chmod -R a+rX "$S3_SMB_TEST_ARTIFACTS"; then
    echo 'Could not make daemon logs readable' >&2
    status=1
  fi
  exit "$status"
}
trap readable_logs EXIT
cd /src
go build -buildvcs=false -o /tmp/s3-smb .
export S3_SMB_E2E_BINARY=/tmp/s3-smb
go test -race -shuffle=on -count=1 -timeout=30m ./...
go test -race -shuffle=on -count=1 -tags smbnext ./internal/app/...

echo '== Samba checks against smbnext =='
go build -race -tags smbnext -buildvcs=false -o /tmp/s3-smb-next .
GORACE=halt_on_error=1 S3_SMB_SAMBA_BINARY=/tmp/s3-smb-next \
  go test -race -shuffle=on -count=1 -timeout=10m -run '^TestSambaInterop$' ./test/e2e

echo '== Chaos checks against smbnext =='
GORACE=halt_on_error=1 S3_SMB_CHAOS_BINARY=/tmp/s3-smb-next \
  S3_SMB_CHAOS_SEED="${S3_SMB_CHAOS_SEED:-}" \
  go test -race -shuffle=on -count=1 -v -timeout="$chaos_timeout" -run '^TestChaos.*$' ./test/e2e
