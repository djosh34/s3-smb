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
# The chaos tests run only against the new server. A given seed replays a run.
chaos_seed=${S3_SMB_CHAOS_SEED:-$(od -An -N8 -tu8 /dev/urandom | tr -d ' ')}
unset S3_SMB_CHAOS_SEED
cd /src
go build -buildvcs=false -o /tmp/s3-smb .
export S3_SMB_E2E_BINARY=/tmp/s3-smb
go test -race -shuffle=on -count=1 -timeout=30m ./...
go test -race -shuffle=on -count=1 -tags smbnext ./internal/app/...

echo '== Integration, Samba and chaos checks against the new server =='
go build -race -tags smbnext -buildvcs=false -o /tmp/s3-smb-next .
echo "Chaos seed $chaos_seed: replay with S3_SMB_CHAOS_SEED=$chaos_seed"
GORACE=halt_on_error=1 S3_SMB_SAMBA=1 S3_SMB_CHAOS_SEED=$chaos_seed S3_SMB_E2E_BINARY=/tmp/s3-smb-next \
  go test -race -shuffle=on -count=1 -timeout=60m ./test/e2e
