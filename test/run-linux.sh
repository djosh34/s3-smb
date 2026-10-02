#!/usr/bin/env bash
set -Eeuo pipefail
cd /src
go mod tidy -diff
go build -buildvcs=false -o /tmp/s3-smb .
export S3_SMB_E2E_BINARY=/tmp/s3-smb
if (( $# == 0 )); then set -- ./...; fi
go test -race -count=1 -timeout=30m "$@"
