#!/usr/bin/env bash
# Internal Docker step of scripts/check.sh.
set -Eeuo pipefail
: "${S3_SMB_CHECK_MODE:?Run scripts/check.sh}"
: "${S3_SMB_E2E_ENDPOINT:?Run scripts/check.sh}"
: "${S3_SMB_TEST_ARTIFACTS:?Run scripts/check.sh}"
artifacts=$S3_SMB_TEST_ARTIFACTS
# The tests run as root. Let the calling user read the daemon logs.
readable_logs() {
  status=$?
  if ! chmod -R a+rX "$artifacts"; then
    echo 'Could not make daemon logs readable' >&2
    status=1
  fi
  exit "$status"
}
trap readable_logs EXIT
cd /src
mkdir -p "$artifacts/default" "$artifacts/smbnext-race"
go build -buildvcs=false -o /tmp/s3-smb .
export S3_SMB_E2E_BINARY=/tmp/s3-smb
export S3_SMB_TEST_ARTIFACTS="$artifacts/default"
go test -race -shuffle=on -count=1 -timeout=30m ./...

# Keep the frozen default server out of the daemon race check until M6.
go build -race -tags smbnext -buildvcs=false -o /tmp/s3-smb-race .
export S3_SMB_E2E_BINARY=/tmp/s3-smb-race
export S3_SMB_TEST_ARTIFACTS="$artifacts/smbnext-race"
export GORACE=halt_on_error=1
race_tests=()
if [[ $S3_SMB_CHECK_MODE == pr ]]; then
  # Cover startup, SMB reads and writes, shutdown, and recovery without a second full run.
  race_tests=(-run '^(TestSMBToS3Smoke|TestFilesystemOperations|TestRecovery)$')
fi
go test -race -tags smbnext -shuffle=on -count=1 -timeout=30m "${race_tests[@]}" ./test/e2e
