#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Runs the Mac acceptance test as root, which Apple's administrative commands
# need. The workflow sets MAC_PHASE and the task paths, and for the b2
# scenario the B2 settings, which sudo passes on from the environment.
set -euo pipefail
cd "$(dirname "$0")/../.."
case "$MAC_PHASE" in
  discover) timeout=25m ;;
  backup) timeout=140m ;;
  recover) timeout=95m ;;
  scenario) timeout=150m ;;
  *) echo 'MAC_PHASE must be discover, backup, recover or scenario' >&2; exit 1 ;;
esac
if [[ ${MAC_SCENARIO:-} == large ]]; then timeout=345m; fi
sudo -n --preserve-env=B2_KEY_ID,B2_APPLICATION_KEY,B2_ENDPOINT,B2_BUCKET /usr/bin/env "PATH=$PATH" "HOME=$HOME" "MAC_RUNNER_HOME=$HOME" \
  "MAC_WORK=$MAC_WORK" "MAC_ARTIFACTS=$MAC_ARTIFACTS" "MAC_TRANSFER=$MAC_TRANSFER" \
  "MAC_PHASE=$MAC_PHASE" "MAC_SCENARIO=${MAC_SCENARIO:-}" \
  "ImageOS=${ImageOS:-unknown}" "ImageVersion=${ImageVersion:-unknown}" \
  GOENV=off GOTOOLCHAIN=local GOWORK=off \
  go test -p 1 -tags macos -count=1 -timeout "$timeout" -run '^TestTimeMachine$' -v ./test/macos/... \
  | tee "$RUNNER_TEMP/mac-harness.log"
