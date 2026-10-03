#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Actions needs sudo for Apple's administrative commands. The test owns everything else.
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${MAC_SERVER:?default or smbnext required}"
case "$MAC_PHASE" in
  discover) timeout=25m ;;
  backup) timeout=140m ;;
  recover) timeout=80m ;;
  scenario) timeout=110m ;;
  *) echo 'MAC_PHASE must be discover, backup, recover or scenario' >&2; exit 1 ;;
esac
sudo -n /usr/bin/env "PATH=$PATH" "HOME=$HOME" "MAC_RUNNER_HOME=$HOME" \
  "MAC_WORK=$MAC_WORK" "MAC_ARTIFACTS=$MAC_ARTIFACTS" "MAC_TRANSFER=$MAC_TRANSFER" \
  "MAC_SERVER=$MAC_SERVER" "MAC_PHASE=$MAC_PHASE" "MAC_SCENARIO=${MAC_SCENARIO:-}" \
  "ImageOS=${ImageOS:-unknown}" "ImageVersion=${ImageVersion:-unknown}" \
  GOMAXPROCS=2 GOFLAGS=-p=2 GOENV=off GOTOOLCHAIN=local GOWORK=off \
  go test -tags macos -count=1 -timeout "$timeout" -v ./test/macos/... \
  | tee "$RUNNER_TEMP/mac-harness.log"
