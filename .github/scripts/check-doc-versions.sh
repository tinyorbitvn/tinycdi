#!/usr/bin/env bash
# check-doc-versions.sh — the published docs describe only the current
# release: no internal version history (v0.x, v1.1, v1.2), release-candidate
# names (rc.<digit>), PR or issue numbers (#NN, #NNN) or v1alpha1. Reviewed
# exceptions (third-party version strings, examples of the tag format) live
# in .github/tests/docs-versions.allow, each with a reason.
#
#   check-doc-versions.sh            # the whole docs scope, stale check on (CI)
#   check-doc-versions.sh FILE...    # only these files (repo-relative), while editing
#
# TCDI_DOCS_ROOT / TCDI_DOCS_ALLOW override the repo root and the allowlist
# (fixtures). Write a release-candidate placeholder as -rc.N, not a digit.
set -uo pipefail
SELF_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ROOT="${TCDI_DOCS_ROOT:-$SELF_ROOT}"
ALLOW="${TCDI_DOCS_ALLOW:-$SELF_ROOT/.github/tests/docs-versions.allow}"
PAT='(^|[^0-9A-Za-z.])v0\.[0-9]|(^|[^0-9A-Za-z.])v1\.[12]([^0-9]|$)|\brc\.[0-9]|(^|[^0-9A-Za-z&/_#-])#[0-9]{2,3}([^0-9A-Za-z_-]|$)|v1alpha1'
ENGINE="$SELF_ROOT/.github/scripts/allowlist-grep.sh"
if [ "$#" -gt 0 ]; then
  exec bash "$ENGINE" -s -p "$PAT" -a "$ALLOW" -r "$ROOT" -- "$@"
fi
exec bash "$ENGINE" -p "$PAT" -a "$ALLOW" -r "$ROOT" -- \
  ':(glob)docs/**/*.md' ':(exclude,glob)docs/superpowers/**' ':(glob)*.md' \
  'deploy/helm/tinycdi/README.md' '.github/README.md'
