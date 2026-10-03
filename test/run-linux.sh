#!/usr/bin/env bash
# Internal Docker step of scripts/check.sh.
set -Eeuo pipefail
: "${S3_SMB_CHECK_MODE:?Run scripts/check.sh}"
: "${S3_SMB_E2E_ENDPOINT:?Run scripts/check.sh}"
: "${S3_SMB_TEST_ARTIFACTS:?Run scripts/check.sh}"
# The tests run as root. Let the calling user read the daemon logs.
trap 'chmod -R a+rX "$S3_SMB_TEST_ARTIFACTS"' EXIT
cd /src
go build -buildvcs=false -o /tmp/s3-smb .
export S3_SMB_E2E_BINARY=/tmp/s3-smb
go test -race -shuffle=on -count=1 -timeout=30m ./...
