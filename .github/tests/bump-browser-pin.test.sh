#!/usr/bin/env bash
# bump-browser-pin.test.sh — regression coverage for the runtime release
# train's pin-bump helper (D27, FX-R15): bumping a Debian browser pin
# (chromium or firefox-esr) must rewrite exactly that engine's pinned
# spots — the ARG in build/browser/Dockerfile and the doc pins in
# docs/images.md and docs/compatibility.md (for firefox-esr that includes
# the locked deb sha256) — leave the other engine and the historical scan
# notes alone, reject malformed input, and be a no-op when the pin is
# already current.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUMP="$ROOT/.github/scripts/bump-browser-pin.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

OLD="154.0.8037.92-1~deb12u1"
NEW="155.0.1.2-1~deb12u1"
FF_OLD="153.4.0esr-1~deb12u1"
FF_NEW="153.5.0esr-1~deb12u1"
SHA_OLD="$(printf 'a%.0s' $(seq 1 64))"
SHA_NEW="$(printf 'b%.0s' $(seq 1 64))"
FILES=(Dockerfile images.md compatibility.md NOTICE)

make_fixtures() {
  cat > "$D/Dockerfile" <<EOT
ARG BASE_IMAGE=ghcr.io/tinyorbitvn/tinycdi-linux-desktop:local
FROM \${BASE_IMAGE}
ARG CHROMIUM_APT_VERSION=$OLD
ARG FIREFOX_ESR_APT_VERSION=$FF_OLD
EOT
  cat > "$D/images.md" <<EOT
- Chromium: \`$OLD\` (apt pin — repin on Debian security updates)
- Firefox ESR (fallback): \`$FF_OLD\`
the trivy gate was clean on \`$OLD\` and \`$FF_OLD\` — a historical scan
note that must keep the versions it was measured against.
EOT
  cat > "$D/compatibility.md" <<EOT
| Firefox ESR fallback | firefox-esr $FF_OLD (deb.debian.org/debian-security bookworm-security), deb sha256 \`$SHA_OLD\` |
- **Chromium apt pin** — runtime Dockerfile installs bookworm Chromium;
  current ARG \`$OLD\` (latest published in
  bookworm-security as of 2026-10-01). Repin on each Debian security
  update.
EOT
  cat > "$D/NOTICE" <<EOT
* Chromium $OLD and Firefox ESR $FF_OLD
  (browser image) — see the Debian package copyright files.
EOT
  for f in "${FILES[@]}"; do
    cp "$D/$f" "$D/$f.orig"
  done
}

rc=0; out=""
run_bump() { # run_bump <engine> <version> [sha]
  local engine="$1" ver="$2" sha="${3:-}"
  out="$(BUMP_DATE=2026-10-20 BUMP_DEB_SHA256="$sha" bash "$BUMP" "$engine" "$ver" \
    "$D/Dockerfile" "$D/images.md" "$D/compatibility.md" "$D/NOTICE" 2>&1)"; rc=$?
}
unchanged() { # unchanged <desc> <file>...
  local desc="$1"; shift
  for f in "$@"; do
    cmp -s "$D/$f.orig" "$D/$f" || { echo "FAIL: $desc: $f was modified"; fails=1; }
  done
}
# every changed line must carry one of the allowed markers
only_pin_lines() { # only_pin_lines <file> <marker>...
  local f="$1"; shift
  local changed stray
  changed="$(diff "$D/$f.orig" "$D/$f" | grep -c '^> ')"
  stray="$(diff "$D/$f.orig" "$D/$f" | grep '^> ')"
  for m in "$@"; do stray="$(printf '%s\n' "$stray" | grep -vF -- "$m")"; done
  if [ "$changed" -lt 1 ] || [ -n "$(printf '%s' "$stray" | tr -d '[:space:]')" ]; then
    echo "FAIL: $f changed outside the pin lines"; diff "$D/$f.orig" "$D/$f"; fails=1
  fi
}

# --- chromium: bumps its spots, and nothing else ---
make_fixtures
run_bump chromium "$NEW"
[ "$rc" -eq 0 ] || { echo "FAIL: chromium bump exited $rc: $out"; fails=1; }
grep -qx "ARG CHROMIUM_APT_VERSION=$NEW" "$D/Dockerfile" \
  || { echo "FAIL: chromium: Dockerfile ARG not repinned"; fails=1; }
grep -qx "ARG FIREFOX_ESR_APT_VERSION=$FF_OLD" "$D/Dockerfile" \
  || { echo "FAIL: chromium bump touched the firefox ARG"; fails=1; }
grep -qF -- "- Chromium: \`$NEW\`" "$D/images.md" \
  || { echo "FAIL: chromium: images.md lacks the new pin"; fails=1; }
grep -qF "current ARG \`$NEW\`" "$D/compatibility.md" \
  || { echo "FAIL: chromium: compatibility.md lacks the new pin"; fails=1; }
grep -qF "clean on \`$OLD\` and \`$FF_OLD\`" "$D/images.md" \
  || { echo "FAIL: chromium: historical scan note was rewritten"; fails=1; }
grep -qF "as of 2026-10-20" "$D/compatibility.md" \
  || { echo "FAIL: chromium: 'as of' date not refreshed"; fails=1; }
grep -qF "firefox-esr $FF_OLD" "$D/compatibility.md" \
  || { echo "FAIL: chromium bump touched the firefox row"; fails=1; }
grep -qF "Chromium $NEW and Firefox ESR $FF_OLD" "$D/NOTICE" \
  || { echo "FAIL: chromium: NOTICE not repinned / firefox touched"; cat "$D/NOTICE"; fails=1; }
only_pin_lines Dockerfile "$NEW"
only_pin_lines images.md "$NEW"
only_pin_lines compatibility.md "$NEW" 'as of 2026-10-20'
only_pin_lines NOTICE "$NEW"

# --- firefox-esr: bumps its spots incl. the deb sha256, and nothing else ---
make_fixtures
run_bump firefox-esr "$FF_NEW" "$SHA_NEW"
[ "$rc" -eq 0 ] || { echo "FAIL: firefox bump exited $rc: $out"; fails=1; }
grep -qx "ARG FIREFOX_ESR_APT_VERSION=$FF_NEW" "$D/Dockerfile" \
  || { echo "FAIL: firefox: Dockerfile ARG not repinned"; fails=1; }
grep -qx "ARG CHROMIUM_APT_VERSION=$OLD" "$D/Dockerfile" \
  || { echo "FAIL: firefox bump touched the chromium ARG"; fails=1; }
grep -qF -- "- Firefox ESR (fallback): \`$FF_NEW\`" "$D/images.md" \
  || { echo "FAIL: firefox: images.md lacks the new pin"; fails=1; }
grep -qF -- "- Chromium: \`$OLD\`" "$D/images.md" \
  || { echo "FAIL: firefox bump touched the chromium pin line"; fails=1; }
grep -qF "clean on \`$OLD\` and \`$FF_OLD\`" "$D/images.md" \
  || { echo "FAIL: firefox: historical scan note was rewritten"; fails=1; }
grep -qF "| firefox-esr $FF_NEW (deb.debian.org/debian-security bookworm-security), deb sha256 \`$SHA_NEW\` |" "$D/compatibility.md" \
  || { echo "FAIL: firefox: compatibility.md row lacks new version + sha"; cat "$D/compatibility.md"; fails=1; }
if grep -qF "$SHA_OLD" "$D/compatibility.md"; then
  echo "FAIL: firefox: old deb sha256 still in compatibility.md"; fails=1
fi
grep -qF "as of 2026-10-01" "$D/compatibility.md" \
  || { echo "FAIL: firefox bump refreshed the chromium 'as of' stamp"; fails=1; }
grep -qF "Chromium $OLD and Firefox ESR $FF_NEW" "$D/NOTICE" \
  || { echo "FAIL: firefox: NOTICE not repinned / chromium touched"; cat "$D/NOTICE"; fails=1; }
only_pin_lines Dockerfile "$FF_NEW"
only_pin_lines images.md "$FF_NEW"
only_pin_lines compatibility.md "$FF_NEW" "$SHA_NEW"
only_pin_lines NOTICE "$FF_NEW"

# --- firefox-esr without a valid deb sha256: refuse, leave everything alone ---
for bad_sha in "" "deadbeef" "$(printf 'G%.0s' $(seq 1 64))"; do
  make_fixtures
  run_bump firefox-esr "$FF_NEW" "$bad_sha"
  [ "$rc" -ne 0 ] || { echo "FAIL: firefox bump accepted sha '$bad_sha'"; fails=1; }
  unchanged "firefox bump with bad sha '$bad_sha'" "${FILES[@]}"
done

# --- no-op when equal: exit 0, byte-identical files ---
make_fixtures
run_bump chromium "$OLD"
[ "$rc" -eq 0 ] || { echo "FAIL: chromium no-op bump exited $rc: $out"; fails=1; }
unchanged "chromium no-op" "${FILES[@]}"
run_bump firefox-esr "$FF_OLD" "$SHA_NEW"
[ "$rc" -eq 0 ] || { echo "FAIL: firefox no-op bump exited $rc: $out"; fails=1; }
unchanged "firefox no-op" "${FILES[@]}"

# --- rejects odd input: non-zero exit, no diff ---
for engine in chromium firefox-esr; do
  for bad in "155.0.1.2-1~deb12u1; rm -rf /" "155.0.1.2 -1" "x'OR'1'='1"; do
    make_fixtures
    run_bump "$engine" "$bad" "$SHA_NEW"
    [ "$rc" -ne 0 ] || { echo "FAIL: $engine accepted malformed version '$bad'"; fails=1; }
    unchanged "$engine rejected '$bad'" "${FILES[@]}"
  done
done
make_fixtures
run_bump "gecko" "$NEW" "$SHA_NEW"
[ "$rc" -ne 0 ] || { echo "FAIL: accepted unknown engine"; fails=1; }
unchanged "unknown engine" "${FILES[@]}"

if [ "$fails" -eq 0 ]; then echo "bump-browser-pin: all cases pass"; fi
exit "$fails"
