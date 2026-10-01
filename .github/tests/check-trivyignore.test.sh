#!/usr/bin/env bash
# check-trivyignore.test.sh — regression coverage for SUPR-3.
#
# Every case below mirrors an evasion found in the internal security
# re-review, plus the happy path and the --out effective-ignorefile
# emission the scan jobs use.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHK="$ROOT/.github/scripts/check-trivyignore.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

accept() { # accept <desc> <file>
  if ! bash "$CHK" "$D/$2" >/dev/null 2>&1; then
    echo "FAIL: $1 should be accepted"; fails=1
  fi
}
reject() { # reject <desc> <file>
  if bash "$CHK" "$D/$2" >/dev/null 2>&1; then
    echo "FAIL: $1 should be rejected"; fails=1
  fi
}

FAR="$(date -d '+20 days' +%F)"      # inside the 30-day horizon
PAST="$(date -d '-1 day' +%F)"
TOOFAR="$(date -d '+90 days' +%F)"

cat > "$D/ok.trivyignore" <<EOF
# reason:   accepted risk — mitigated by sandbox
# approver: reviewer1
# expires:  $FAR
CVE-2026-95282
EOF
accept "ok" ok.trivyignore

printf '# reason: x\n# approver: y\n# expires: %s\nCVE-2026-95282\r\n' "$FAR" \
  > "$D/crlf.trivyignore"
accept "crlf tolerated" crlf.trivyignore

cat > "$D/expired.trivyignore" <<EOF
# reason: x
# approver: y
# expires: $PAST
CVE-2026-95282
EOF
reject "expired entry" expired.trivyignore

cat > "$D/horizon.trivyignore" <<EOF
# reason: x
# approver: y
# expires: $TOOFAR
CVE-2026-95282
EOF
reject "beyond 30-day horizon" horizon.trivyignore

cat > "$D/empty-fields.trivyignore" <<EOF
# reason:
# approver:
# expires: $FAR
CVE-2026-95282
EOF
reject "empty reason/approver" empty-fields.trivyignore

cat > "$D/fuzzy-date.trivyignore" <<EOF
# reason: x
# approver: y
# expires: next year
CVE-2026-95282
EOF
reject "non-ISO expiry" fuzzy-date.trivyignore

cat > "$D/multi.trivyignore" <<EOF
# reason: x
# approver: y
# expires: $FAR
CVE-2026-95282 CVE-2026-95293
EOF
reject "multi-id line" multi.trivyignore

cat > "$D/indent.trivyignore" <<EOF
  # reason: x
# approver: y
# expires: $FAR
CVE-2026-95282
EOF
reject "indented comment does not count" indent.trivyignore

cat > "$D/nativeexp-mismatch.trivyignore" <<EOF
# reason: x
# approver: y
# expires: $FAR
CVE-2026-95282 exp:2999-01-01
EOF
reject "inline exp disagrees with comment" nativeexp-mismatch.trivyignore

# --out: an expired entry must FAIL the check — --out is only reachable on
# a fully-valid file. A valid file's entries all emit with native exp:.
cat > "$D/emit.trivyignore" <<EOF
# reason: keep
# approver: y
# expires: $FAR
CVE-2026-95282
EOF
if bash "$CHK" --out "$D/effective" "$D/emit.trivyignore" \
  && grep -qx "CVE-2026-95282 exp:$FAR" "$D/effective"; then
  :
else
  echo "FAIL: --out should emit 'CVE-2026-95282 exp:$FAR'"; fails=1
fi
# absent source -> empty effective file, still rc 0
if ! bash "$CHK" --out "$D/effective-absent" "$D/does-not-exist"; then
  echo "FAIL: absent ignorefile should pass and emit an empty file"; fails=1
fi

if [ "$fails" -eq 0 ]; then echo "check-trivyignore: all cases pass"; fi
exit "$fails"
