#!/usr/bin/env bash
# Internal Docker step of scripts/check.sh.
set -Eeuo pipefail
: "${S3_SMB_CHECK_MODE:?Run scripts/check.sh}"
: "${S3_SMB_E2E_ENDPOINT:?Run scripts/check.sh}"
: "${S3_SMB_TEST_ARTIFACTS:?Run scripts/check.sh}"
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

echo '== Samba checks against smbnext =='
go build -race -tags smbnext -buildvcs=false -o /tmp/s3-smb-next .
S3_SMB_SAMBA_BINARY=/tmp/s3-smb-next \
  go test -race -shuffle=on -count=1 -timeout=10m -run '^TestSambaInterop$' ./test/e2e
