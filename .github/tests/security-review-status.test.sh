#!/usr/bin/env bash
# security-review-status.test.sh — the v1.0 security review is closed out:
# every finding row in docs/security/review-v1.0.md §4 carries a final status
# ("fixed (<commit subject>)" or "accepted (owner)"), the public entry points
# link the report, and every test the inventory's review section names exists.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REV="$ROOT/docs/security/review-v1.0.md"
INV="$ROOT/docs/security/test-inventory.md"
fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }

[ -f "$REV" ] || { echo "FAIL: $REV is missing"; exit 1; }

# 1. Every finding row (| R1-NN | Sev | Row | Title | Where | Status |) has a
#    final status in its last cell.
n=0
while IFS= read -r row; do
  n=$((n + 1))
  id="$(sed -E 's/^\| (R1-[0-9]{2}) .*/\1/' <<<"$row")"
  status="$(sed -E 's/.*\| ([^|]*[^| ]) *\|[[:space:]]*$/\1/' <<<"$row")"
  case "$status" in
    'fixed ('*')'*|'accepted (owner)'*) ;;
    *) fail "$id status is '$status', want 'fixed (<commit subject>)' or 'accepted (owner)'" ;;
  esac
done < <(grep -E '^\| R1-[0-9]{2} \|' "$REV")
[ "$n" -eq 41 ] || fail "found $n finding rows in review-v1.0.md, want 41"
grep -qE 'All findings are open' "$REV" && fail "review-v1.0.md header still says all findings are open"

# 2. The public entry points link the report.
for f in README.md SECURITY.md .github/README.md; do
  grep -qE '\]\((\.\./)?docs/security/review-v1\.0\.md(#[a-z0-9-]+)?\)' "$ROOT/$f" \
    || fail "$f does not link docs/security/review-v1.0.md"
done

# 3. The inventory's review section covers every finding, and every Go test it
#    names exists.
sec="$(awk '/^## 12\. Security-review fix tests/{f=1; next} f && /^## /{exit} f' "$INV")"
if [ -z "$sec" ]; then
  fail "test-inventory.md has no '## 12. Security-review fix tests' section"
else
  for i in $(seq 1 41); do
    id="$(printf 'R1-%02d' "$i")"
    grep -qE "\| $id( |,)" <<<"$sec" || fail "inventory review section has no row for $id"
  done
  while IFS= read -r t; do
    git -C "$ROOT" grep -qE "^func $t\(" -- '*.go' || fail "inventory names $t, which no Go file defines"
  done < <(grep -oE '`Test[A-Za-z0-9_]+`' <<<"$sec" | tr -d '`' | sort -u)
fi

exit "$fails"
