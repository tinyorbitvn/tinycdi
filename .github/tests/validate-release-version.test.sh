#!/usr/bin/env bash
# Regression test: release version validation must reject newline/crud
# injection that a line-based grep would accept (multi-line version input).
set -uo pipefail

SCRIPT="$(dirname "$0")/../scripts/validate-release-version.sh"
fails=0

accept() {
  if ! VERSION="$1" bash "$SCRIPT" >/dev/null 2>&1; then
    echo "FAIL: '$1' should be accepted"; fails=1
  fi
}
reject() {
  if VERSION="$1" bash "$SCRIPT" >/dev/null 2>&1; then
    echo "FAIL: $(printf '%q' "$1") should be rejected"; fails=1
  fi
}

accept "v1.2.3"
accept "v0.0.0-dryrun"
accept "v1.2.3-rc.1"
accept "v10.20.30-alpha.2"

reject "1.2.3"                      # missing v prefix
reject "v1.2"                       # not semver
reject "v1.2.3 extra"               # trailing junk
reject "$(printf 'v1.2.3\nINJECTED_KEY=attacker-value')"   # SEC-43 PoC
reject "$(printf 'v1.2.3\r\nX=1')"
reject "$(printf '\nv1.2.3')"       # leading newline
reject ""                           # empty
reject "v1.2.3-x_1"                 # invalid char in prerelease

if [ "$fails" -eq 0 ]; then echo "validate-release-version: all cases pass"; fi
exit "$fails"
