#!/usr/bin/env bash
# bump-chromium-pin.test.sh — regression coverage for the runtime release
# train's pin-bump helper (D27): a Debian chromium pin bump must rewrite
# exactly the three pinned spots — the CHROMIUM_APT_VERSION ARG in
# build/browser/Dockerfile and the doc pins in docs/images.md and
# docs/compatibility.md — reject malformed versions, and be a no-op when
# the pin is already current.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUMP="$ROOT/.github/scripts/bump-chromium-pin.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

OLD="154.0.8037.92-1~deb12u1"
NEW="155.0.1.2-1~deb12u1"

make_fixtures() {
  cat > "$D/Dockerfile" <<EOF
ARG BASE_IMAGE=ghcr.io/tinyorbitvn/tinycdi-linux-desktop:local
FROM \${BASE_IMAGE}
ARG CHROMIUM_APT_VERSION=$OLD
ARG FIREFOX_ESR_APT_VERSION=140.16.0esr-1~deb12u1
EOF
  cat > "$D/images.md" <<EOF
- Chromium: \`$OLD\` (apt pin — repin on Debian security updates)
- Firefox ESR (fallback): \`140.16.0esr-1~deb12u1\`
the trivy gate was clean on \`$OLD\` — a historical scan note that must
keep the version it was measured against.
EOF
  cat > "$D/compatibility.md" <<EOF
- **Chromium apt pin** — runtime Dockerfile installs bookworm Chromium;
  current ARG \`$OLD\` (latest published in
  bookworm-security as of 2026-10-01). Repin on each Debian security
  update.
EOF
  for f in Dockerfile images.md compatibility.md; do
    cp "$D/$f" "$D/$f.orig"
  done
}

rc=0; out=""
run_bump() {
  out="$(BUMP_DATE=2026-10-20 bash "$BUMP" "$@" 2>&1)"; rc=$?
}

# --- bumps all three files, and nothing else ---
make_fixtures
run_bump "$NEW" "$D/Dockerfile" "$D/images.md" "$D/compatibility.md"
[ "$rc" -eq 0 ] || { echo "FAIL: bump exited $rc: $out"; fails=1; }
grep -qx "ARG CHROMIUM_APT_VERSION=$NEW" "$D/Dockerfile" \
  || { echo "FAIL: Dockerfile ARG not repinned"; fails=1; }
grep -qF "\`$NEW\`" "$D/images.md" \
  || { echo "FAIL: images.md lacks the new pin"; fails=1; }
grep -qF "\`$NEW\`" "$D/compatibility.md" \
  || { echo "FAIL: compatibility.md lacks the new pin"; fails=1; }
# The historical scan note keeps the version it measured.
grep -qF "clean on \`$OLD\`" "$D/images.md" \
  || { echo "FAIL: historical pin reference was rewritten"; fails=1; }
# The doc "as of" stamp tracks the bump date.
grep -qF "as of 2026-10-20" "$D/compatibility.md" \
  || { echo "FAIL: compatibility.md 'as of' date not refreshed"; fails=1; }
# Nothing else changed: every changed line carries the new pin or the
# refreshed 'as of' date.
for f in Dockerfile images.md compatibility.md; do
  changed="$(diff "$D/$f.orig" "$D/$f" | grep -c '^> ')"
  stray="$(diff "$D/$f.orig" "$D/$f" | grep '^> ' \
    | grep -vF "$NEW" | grep -vcF 'as of 2026-10-20')"
  if [ "$changed" -lt 1 ] || [ "$stray" -ne 0 ]; then
    echo "FAIL: $f changed outside the pin/'as of' lines"
    diff "$D/$f.orig" "$D/$f"
    fails=1
  fi
done

# --- no-op when equal: exit 0, byte-identical files ---
make_fixtures
run_bump "$OLD" "$D/Dockerfile" "$D/images.md" "$D/compatibility.md"
[ "$rc" -eq 0 ] || { echo "FAIL: no-op bump exited $rc: $out"; fails=1; }
for f in Dockerfile images.md compatibility.md; do
  cmp -s "$D/$f.orig" "$D/$f" \
    || { echo "FAIL: no-op bump modified $f"; fails=1; }
done

# --- rejects odd input: non-zero exit, no diff ---
for bad in "155.0.1.2-1~deb12u1; rm -rf /" "155.0.1.2 -1" "x'OR'1'='1"; do
  make_fixtures
  run_bump "$bad" "$D/Dockerfile" "$D/images.md" "$D/compatibility.md"
  if [ "$rc" -eq 0 ]; then
    echo "FAIL: accepted malformed version '$bad'"; fails=1
  fi
  for f in Dockerfile images.md compatibility.md; do
    cmp -s "$D/$f.orig" "$D/$f" \
      || { echo "FAIL: rejected input still modified $f"; fails=1; }
  done
done

if [ "$fails" -eq 0 ]; then echo "bump-chromium-pin: all cases pass"; fi
exit "$fails"
