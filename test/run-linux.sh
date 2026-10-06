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
# S3_SMB_JOB picks what this run covers: rest is every test but the chaos and
# break tests, a test name runs that test, and NAME-ROUND runs one round of a
# break test. Without it everything runs.
job=${S3_SMB_JOB:-all}
export GORACE=halt_on_error=1 S3_SMB_E2E_BINARY=/tmp/s3-smb
case $job in
  all) go test -race -shuffle=on -count=1 "-timeout=$timeout" ./internal/engine ./test/e2e ;;
  rest) go test -race -shuffle=on -count=1 "-timeout=$timeout" -skip '^(TestBreak|TestChaos)' ./internal/engine ./test/e2e ;;
  Test*-*) S3_SMB_ROUND=${job##*-} go test -race -count=1 "-timeout=$timeout" -run "^${job%-*}\$" ./test/e2e ;;
  Test*) go test -race -count=1 "-timeout=$timeout" -run "^${job}\$" ./test/e2e ;;
  *) echo "Unknown job $job" >&2; exit 2 ;;
esac
