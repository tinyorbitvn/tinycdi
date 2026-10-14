#!/usr/bin/env bash
# security-doc-claims.test.sh — pins the security claims in prose that the
# v1.0 security review (docs/security/review-v1.0.md) corrected. Each line
# names its review finding and states what a document must say (need), must
# not say again (absent), or what a threat-model register row must carry
# (need_row). Files are flattened to one line with every whitespace run
# squeezed to one space, so a claim may wrap anywhere in the source.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TM="docs/security/threat-model.md"
fails=0

flat() { tr -s '[:space:]' ' ' < "$ROOT/$1"; }

need() { # need <finding> <repo-relative file> <ERE>
  if [ ! -f "$ROOT/$2" ]; then
    echo "FAIL: $1: $2 does not exist"; fails=$((fails + 1)); return
  fi
  if ! flat "$2" | grep -qE -- "$3"; then
    echo "FAIL: $1: $2 does not state /$3/"; fails=$((fails + 1))
  fi
}

absent() { # absent <finding> <repo-relative file> <ERE>
  if [ ! -f "$ROOT/$2" ]; then
    echo "FAIL: $1: $2 does not exist"; fails=$((fails + 1)); return
  fi
  if flat "$2" | grep -qE -- "$3"; then
    echo "FAIL: $1: $2 still states /$3/"; fails=$((fails + 1))
  fi
}

need_row() { # need_row <finding> <TM-NN> <ERE>
  local row
  row="$(grep -E "^\| $2 \|" "$ROOT/$TM")"
  if [ -z "$row" ]; then
    echo "FAIL: $1: $TM has no register row $2"; fails=$((fails + 1)); return
  fi
  if ! printf '%s\n' "$row" | grep -qE -- "$3"; then
    echo "FAIL: $1: register row $2 does not state /$3/"; fails=$((fails + 1))
  fi
}

# R1-17: a removed tenant admin keeps the role until the next sign-in; the
# docs must say so and name the immediate remedy.
need_row R1-17 TM-11 'next sign-in'
need_row R1-17 TM-11 'users/\{ownerRef\}/sessions:revoke-all'
need R1-17 SECURITY.md 'admin group at the IdP takes effect at their next sign-in'
need R1-17 SECURITY.md 'users/\{ownerRef\}/sessions:revoke-all'

# R1-21: the server does not require a dry run before a confirmed :apply.
need_row R1-21 TM-17 'the API requires an explicit `dryRun` flag'
absent R1-21 "$TM" 'it runs a dry run first'
absent R1-21 internal/api/openapi.yaml 'Always dry run first'
need R1-21 internal/api/openapi.yaml 'The server does not require one'

# R1-27: the in-pod trust residual names the desktop leg everywhere it is stated.
need_row R1-27 TM-37 'desktop leg'
need_row R1-27 TM-37 'bind an unmodified runtime only'
need     R1-27 docs/security/threat-model.md 'TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition'
need     R1-27 docs/releases/v1.0.0.md 'over the desktop leg'
absent   R1-27 docs/releases/v1.0.0.md 'Net effect: a tampered helper can mishandle only'
need     R1-27 docs/runbooks/session-policy.md 'over the desktop leg'
need     R1-27 SECURITY.md 'Clipboard, file-transfer and other in-desktop restrictions are UX defaults, not DLP'

exit "$fails"
