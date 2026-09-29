#!/usr/bin/env bash
set -Eeuo pipefail
export GOMAXPROCS=2
mkdir -p /artifacts
cd /src
mode=${1:-suite}; shift || true
status=0
run() {
  local name=$1; shift
  printf '\n=== %s ===\n' "$name"
  if "$@" 2>&1 | tee "/artifacts/$name.log"; then
    printf '%s\tPASS\n' "$name" >>/artifacts/phases.tsv
  else
    status=1
    printf '%s\tFAIL\n' "$name" >>/artifacts/phases.tsv
  fi
}
go version | tee /artifacts/toolchain.txt
uname -a >>/artifacts/toolchain.txt
if (( $# > 0 )); then
  packages=("$@")
else
  go list ./... > /artifacts/packages.txt
  mapfile -t packages < <(grep -v '/test/e2e$' /artifacts/packages.txt)
fi
if (( ${#packages[@]} == 0 )); then
  echo 'No buildable packages; refusing an empty successful suite.' >&2
  exit 1
fi
if [[ $mode == unit || $mode == suite || $mode == release ]]; then
  run unit go test -p 2 -count=1 -timeout=15m -json "${packages[@]}"
fi
if [[ $mode == race || $mode == suite || $mode == release ]]; then
  run race go test -race -p 2 -count=1 -timeout=20m -json "${packages[@]}"
fi
if [[ $mode == transport || $mode == e2e || $mode == suite || $mode == release ]]; then
  run transport bash /src/test/transport/setup.sh go test -p 2 -count=1 -timeout=3m -json ./internal/storage -run '^TestTransportAcceptance$'
fi
if [[ $mode == e2e || $mode == suite || $mode == release ]]; then
  run packaging bash /src/scripts/check-packaging.sh
  run build go build -p 2 -trimpath -o /artifacts/s3-smb .
  export S3_SMB_E2E_BINARY=/artifacts/s3-smb
  run e2e go test -p 2 -count=1 -timeout=30m -json ./test/e2e
fi
# A useful green targeted/suite run is not release approval. The separate gate
# requires execution evidence for every accepted contract row, never skips.
if [[ $mode == release ]]; then
  for name in unit race transport e2e; do [[ ! -f /artifacts/$name.log ]] || cat "/artifacts/$name.log"; done > /artifacts/all-tests.json
  run coverage go run -p 2 ./test/coverage /artifacts/all-tests.json test/coverage/required.tsv
fi
if [[ ${S3_SMB_TEST_INJECT_FAILURE:-} == 1 ]]; then
  echo 'Deliberate artifact-path failure requested.' | tee /artifacts/injected-failure.txt
  status=1
fi
exit "$status"
