#!/usr/bin/env bash
# check-browser-freshness.test.sh — regression coverage for the browser
# image's engine freshness gate (SUPR-4, FX-R15). Both pinned engines are
# covered — chromium AND firefox-esr — against fixture Packages indexes
# (BROWSER_FRESHNESS_INDEX), no network:
#
#   both current            -> exit 0, no BUMP line
#   firefox-esr stale       -> exit 1, "newer available", BUMP firefox-esr
#   chromium stale          -> exit 1, "newer available", BUMP chromium
#   both stale              -> exit 1, BUMP for each
#   pinned version gone     -> exit 1, reported explicitly as
#                              "pinned version gone" (bookworm-security
#                              keeps only the newest build, so this is
#                              what a stale pin really looks like and the
#                              case that broke the images workflow), not
#                              merely "newer available"
#   check cannot run        -> exit 2 (package absent / pin missing)
#
# and that the BUMP lines feed bump-browser-pin.sh into a consistent repin.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHECK="$ROOT/.github/scripts/check-browser-freshness.sh"
BUMP="$ROOT/.github/scripts/bump-browser-pin.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

CHR_PIN="154.0.8037.92-1~deb12u1"
CHR_NEW="155.0.8100.10-1~deb12u1"
FF_PIN="153.4.0esr-1~deb12u1"
FF_NEW="153.5.0esr-1~deb12u1"
SHA_FF_PIN="$(printf 'a%.0s' $(seq 1 64))"
SHA_FF_NEW="$(printf 'b%.0s' $(seq 1 64))"
SHA_CHR_NEW="$(printf 'c%.0s' $(seq 1 64))"

cat > "$D/Dockerfile" <<EOT
ARG BASE_IMAGE=ghcr.io/tinyorbitvn/tinycdi-linux-desktop:local
ARG CHROMIUM_APT_VERSION=$CHR_PIN
ARG FIREFOX_ESR_APT_VERSION=$FF_PIN
EOT

stanza() { # stanza <package> <version> <sha256>
  printf 'Package: %s\nVersion: %s\nArchitecture: amd64\nSHA256: %s\n\n' "$1" "$2" "$3"
}
# Noise packages that must not be mistaken for the engines (firefox-esr-l10n
# shares a prefix; chromium-sandbox shares one with chromium).
noise() {
  stanza firefox-esr-l10n-de 999.0esr-1~deb12u1 "$(printf 'd%.0s' $(seq 1 64))"
  stanza chromium-sandbox 999.0.0.1-1~deb12u1 "$(printf 'd%.0s' $(seq 1 64))"
  stanza curl 7.88.1-10+deb12u99 "$(printf 'd%.0s' $(seq 1 64))"
}

rc=0; out=""
run_check() { # run_check <index-file>
  out="$(BROWSER_FRESHNESS_INDEX="$1" bash "$CHECK" "$D/Dockerfile" 2>&1)"; rc=$?
}
expect() { # expect <desc> <want-rc>
  if [ "$rc" -ne "$2" ]; then
    echo "FAIL: $1: exit $rc, want $2"; echo "$out" | sed 's/^/    /'; fails=1
  fi
}
has() { # has <desc> <fixed-string>
  printf '%s\n' "$out" | grep -qF -- "$2" \
    || { echo "FAIL: $1: output lacks '$2'"; echo "$out" | sed 's/^/    /'; fails=1; }
}
lacks() { # lacks <desc> <fixed-string>
  if printf '%s\n' "$out" | grep -qF -- "$2"; then
    echo "FAIL: $1: output unexpectedly has '$2'"; echo "$out" | sed 's/^/    /'; fails=1
  fi
}

# --- both current -> pass ---
{ noise; stanza chromium "$CHR_PIN" "$SHA_CHR_NEW"; stanza firefox-esr "$FF_PIN" "$SHA_FF_PIN"; } > "$D/idx"
run_check "$D/idx"
expect "both current" 0
has   "both current" "chromium pin is current"
has   "both current" "firefox-esr pin is current"
lacks "both current" "BUMP "

# --- firefox stale (pin still indexed, newer exists) -> fail + bump ---
{ noise; stanza chromium "$CHR_PIN" x; stanza firefox-esr "$FF_PIN" "$SHA_FF_PIN"; stanza firefox-esr "$FF_NEW" "$SHA_FF_NEW"; } > "$D/idx"
run_check "$D/idx"
expect "firefox stale" 1
has   "firefox stale" "firefox-esr: newer available"
has   "firefox stale" "BUMP firefox-esr $FF_NEW $SHA_FF_NEW"
lacks "firefox stale" "pinned version gone"
lacks "firefox stale" "BUMP chromium"
has   "firefox stale" "chromium pin is current"

# --- chromium stale -> fail + bump ---
{ noise; stanza chromium "$CHR_PIN" x; stanza chromium "$CHR_NEW" "$SHA_CHR_NEW"; stanza firefox-esr "$FF_PIN" "$SHA_FF_PIN"; } > "$D/idx"
run_check "$D/idx"
expect "chromium stale" 1
has   "chromium stale" "chromium: newer available"
has   "chromium stale" "BUMP chromium $CHR_NEW $SHA_CHR_NEW"
lacks "chromium stale" "pinned version gone"
lacks "chromium stale" "BUMP firefox-esr"
has   "chromium stale" "firefox-esr pin is current"

# --- both stale -> both reported, one BUMP each ---
{ noise; stanza chromium "$CHR_PIN" x; stanza chromium "$CHR_NEW" "$SHA_CHR_NEW"; stanza firefox-esr "$FF_PIN" "$SHA_FF_PIN"; stanza firefox-esr "$FF_NEW" "$SHA_FF_NEW"; } > "$D/idx"
run_check "$D/idx"
expect "both stale" 1
has "both stale" "BUMP chromium $CHR_NEW"
has "both stale" "BUMP firefox-esr $FF_NEW"

# --- pinned firefox version gone (today's case: only the newer build is
#     indexed) -> reported explicitly, with the bump target ---
{ noise; stanza chromium "$CHR_PIN" x; stanza firefox-esr "$FF_NEW" "$SHA_FF_NEW"; } > "$D/idx"
run_check "$D/idx"
expect "firefox pin gone" 1
has   "firefox pin gone" "firefox-esr: pinned version gone"
has   "firefox pin gone" "$FF_PIN no longer exists in bookworm-security"
has   "firefox pin gone" "BUMP firefox-esr $FF_NEW $SHA_FF_NEW"
lacks "firefox pin gone" "newer available"
lacks "firefox pin gone" "BUMP chromium"

# --- pinned chromium version gone ---
{ noise; stanza chromium "$CHR_NEW" "$SHA_CHR_NEW"; stanza firefox-esr "$FF_PIN" "$SHA_FF_PIN"; } > "$D/idx"
run_check "$D/idx"
expect "chromium pin gone" 1
has   "chromium pin gone" "chromium: pinned version gone"
has   "chromium pin gone" "BUMP chromium $CHR_NEW $SHA_CHR_NEW"
lacks "chromium pin gone" "BUMP firefox-esr"

# --- the check itself cannot run: exit 2, never a bump ---
{ noise; stanza chromium "$CHR_PIN" x; } > "$D/idx"
run_check "$D/idx"
expect "firefox-esr absent from index" 2
lacks  "firefox-esr absent from index" "BUMP "
{ noise; stanza chromium "$CHR_PIN" x; stanza firefox-esr "$FF_PIN" y; } > "$D/idx"
grep -v '^ARG FIREFOX_ESR_APT_VERSION=' "$D/Dockerfile" > "$D/Dockerfile.nofx"
out="$(BROWSER_FRESHNESS_INDEX="$D/idx" bash "$CHECK" "$D/Dockerfile.nofx" 2>&1)"; rc=$?
expect "pin missing from Dockerfile" 2
has    "pin missing from Dockerfile" "FIREFOX_ESR_APT_VERSION not found"

# --- the desktop image's firefox pin is covered: it must equal the browser's ---
{ noise; stanza chromium "$CHR_PIN" x; stanza firefox-esr "$FF_PIN" "$SHA_FF_PIN"; } > "$D/idx"
printf 'ARG FIREFOX_ESR_APT_VERSION=%s\n' "$FF_PIN" > "$D/Dockerfile.desktop"
out="$(BROWSER_FRESHNESS_INDEX="$D/idx" bash "$CHECK" "$D/Dockerfile" "$D/Dockerfile.desktop" 2>&1)"; rc=$?
expect "desktop pin equals browser pin" 0
printf 'ARG FIREFOX_ESR_APT_VERSION=%s\n' "$FF_NEW" > "$D/Dockerfile.desktop"
out="$(BROWSER_FRESHNESS_INDEX="$D/idx" bash "$CHECK" "$D/Dockerfile" "$D/Dockerfile.desktop" 2>&1)"; rc=$?
expect "desktop pin differs from browser pin" 2
has    "desktop pin differs from browser pin" "share one firefox-esr pin"
lacks  "desktop pin differs from browser pin" "BUMP "
: > "$D/Dockerfile.desktop"
out="$(BROWSER_FRESHNESS_INDEX="$D/idx" bash "$CHECK" "$D/Dockerfile" "$D/Dockerfile.desktop" 2>&1)"; rc=$?
expect "desktop pin missing" 2
has    "desktop pin missing" "FIREFOX_ESR_APT_VERSION not found in $D/Dockerfile.desktop"

# --- check -> bump round trip: the BUMP lines repin a stale fixture so a
#     second check passes ---
cat > "$D/images.md" <<EOT
- Chromium: \`$CHR_PIN\` (apt pin)
- Firefox ESR (fallback): \`$FF_PIN\`
EOT
cat > "$D/compat.md" <<EOT
| Firefox ESR fallback | firefox-esr $FF_PIN (deb.debian.org/debian-security bookworm-security), deb sha256 \`$SHA_FF_PIN\` |
- **Chromium apt pin** — current ARG \`$CHR_PIN\` (latest published in
  bookworm-security as of 2026-10-01).
EOT
{ noise; stanza chromium "$CHR_NEW" "$SHA_CHR_NEW"; stanza firefox-esr "$FF_NEW" "$SHA_FF_NEW"; } > "$D/idx"
run_check "$D/idx"
expect "round trip: stale" 1
printf '%s\n' "$out" | while read -r tag engine ver sha; do
  [ "$tag" = BUMP ] || continue
  BUMP_DEB_SHA256="$sha" bash "$BUMP" "$engine" "$ver" "$D/Dockerfile" "$D/images.md" "$D/compat.md" >/dev/null 2>&1 \
    || echo "FAIL: round trip: bump $engine exited non-zero" >> "$D/roundtrip.fail"
done
[ ! -e "$D/roundtrip.fail" ] || { cat "$D/roundtrip.fail"; fails=1; }
run_check "$D/idx"
expect "round trip: current after bump" 0
grep -qF "deb sha256 \`$SHA_FF_NEW\`" "$D/compat.md" \
  || { echo "FAIL: round trip: compatibility.md deb sha256 not refreshed"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "check-browser-freshness: all cases pass"; fi
exit "$fails"
