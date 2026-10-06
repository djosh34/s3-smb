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
# A given seed replays the chaos tests.
export S3_SMB_CHAOS_SEED=${S3_SMB_CHAOS_SEED:-$(od -An -N8 -tu8 /dev/urandom | tr -d ' ')}
# The gate runs the full-length outage and chaos tests.
timeout=60m
if [[ $S3_SMB_CHECK_MODE == gate ]]; then
  timeout=120m
fi
cd /src
go build -race -buildvcs=false -o /tmp/s3-smb .
echo "Chaos seed $S3_SMB_CHAOS_SEED: replay with S3_SMB_CHAOS_SEED=$S3_SMB_CHAOS_SEED"
# Only these packages need MinIO. The unit part of scripts/check.sh runs the rest.
# S3_SMB_SHARD splits them: shard 1 runs all but the break tests, and every
# later shard every (SHARDS-1)-th break test.
shard=${S3_SMB_SHARD:-1/1}
index=${shard%/*} shards=${shard#*/}
export GORACE=halt_on_error=1 S3_SMB_E2E_BINARY=/tmp/s3-smb
if (( shards == 1 )); then
  go test -race -shuffle=on -count=1 "-timeout=$timeout" ./internal/engine ./test/e2e
elif (( index == 1 )); then
  go test -race -shuffle=on -count=1 "-timeout=$timeout" -skip '^TestBreak' ./internal/engine ./test/e2e
else
  tests=$(go test -list '^TestBreak' ./test/e2e)
  selected=()
  position=0
  while IFS= read -r test; do
    if [[ $test == TestBreak* ]]; then
      if (( position % (shards - 1) == index - 2 )); then selected+=("$test"); fi
      position=$((position + 1))
    fi
  done <<< "$tests"
  if (( ${#selected[@]} == 0 )); then
    echo "No break tests for shard $shard"
    exit 0
  fi
  pattern=$(IFS='|'; echo "${selected[*]}")
  go test -race -count=1 "-timeout=$timeout" -run "^($pattern)\$" ./test/e2e
fi
