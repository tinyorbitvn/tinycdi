#!/usr/bin/env bash
# validate-release-version.sh — validate the release version string.
#
# Reads VERSION from the environment and requires a full-string match for
#   v<semver>          e.g. v1.2.3
#   v<semver>-<pre>    e.g. v1.2.3-rc.1
#
# SEC-43: the check uses a bash =~ anchored regex on the whole string — a
# line-based grep accepts "v1.2.3\nEVIL=1" (first line matches) and the extra
# lines would land in GITHUB_OUTPUT/GITHUB_ENV. =~ never matches a string
# containing a newline because anchors bind to the whole string.
set -euo pipefail

: "${VERSION:?VERSION env var is required}"

if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "::error::version '$VERSION' is not v<semver> (v1.2.3 or v1.2.3-rc.1)"
  exit 1
fi
