#!/usr/bin/env bash
# bump-chromium-pin.sh — repin the browser image's chromium apt pin and
# its two doc mirrors (D27). runtime-freshness.yml runs this after
# check-chromium-freshness.sh reports a newer bookworm-security build,
# then opens the resulting diff as a PR.
#
# Rewrites exactly:
#   - `ARG CHROMIUM_APT_VERSION=` in build/browser/Dockerfile
#   - the `- Chromium: `X`` pin line in docs/images.md (the "Known
#     limitations" scan notes keep the version they measured — they are
#     historical, not a pin)
#   - the `current ARG `X`` line in docs/compatibility.md, and refreshes
#     that bullet's `as of` stamp to the bump date
#
# Usage: bump-chromium-pin.sh <new-version> [dockerfile] [images.md] [compatibility.md]
# Env:   BUMP_DATE — 'as of' stamp written to compatibility.md
#        (default: today, UTC)
set -euo pipefail

NEW="${1:-}"
DOCKERFILE="${2:-build/browser/Dockerfile}"
IMAGES_MD="${3:-docs/images.md}"
COMPAT_MD="${4:-docs/compatibility.md}"
STAMP="${BUMP_DATE:-$(date -u +%F)}"

[ -n "$NEW" ] || {
  echo "usage: bump-chromium-pin.sh <new-version> [dockerfile] [images.md] [compatibility.md]" >&2
  exit 2
}

# Debian-version charset only — the value lands in a Dockerfile ARG, in
# markdown and in a git branch name; anything outside this set is either
# malformed or an injection attempt (spaces, ';', quotes, '/', '&').
[[ "$NEW" =~ ^[0-9][A-Za-z0-9.+:~_-]*$ ]] \
  || { echo "::error::refusing malformed chromium version '$NEW'"; exit 1; }

OLD="$(awk -F= '/^ARG CHROMIUM_APT_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
[ -n "$OLD" ] \
  || { echo "::error::CHROMIUM_APT_VERSION not found in $DOCKERFILE"; exit 1; }
if [ "$OLD" = "$NEW" ]; then
  echo "chromium pin already at $NEW — no-op"
  exit 0
fi
echo "bumping chromium pin $OLD -> $NEW"

# Literal substitution inside backtick quotes — the pins carry '.'/'~'
# and awk's index() avoids regex surprises; only pin-declaring lines are
# touched so historical scan notes keep their measured version.
repin_line() { # repin_line <file> <awk line-selector> <also-stamp-as-of:yes|no>
  awk -v old="$OLD" -v new="$NEW" -v stamp="$STAMP" -v dostamp="$3" '
    {
      if ('"$2"') {
        o = "`" old "`"; n = "`" new "`"
        while ((i = index($0, o)) > 0)
          $0 = substr($0, 1, i - 1) n substr($0, i + length(o))
      }
      if (dostamp == "yes" && $0 ~ /bookworm-security as of [0-9]{4}-[0-9]{2}-[0-9]{2}/)
        sub(/as of [0-9]{4}-[0-9]{2}-[0-9]{2}/, "as of " stamp)
      print
    }' "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}

awk -v new="$NEW" '
  /^ARG CHROMIUM_APT_VERSION=/ { $0 = "ARG CHROMIUM_APT_VERSION=" new }
  { print }' "$DOCKERFILE" > "$DOCKERFILE.tmp" && mv "$DOCKERFILE.tmp" "$DOCKERFILE"

repin_line "$IMAGES_MD" '$0 ~ /^- Chromium:/' no
repin_line "$COMPAT_MD" '$0 ~ /current ARG/' yes

# Fail loudly when a pin line drifts out of shape — better a red
# freshness job than a PR that bumps only two of the three pins.
for f in "$DOCKERFILE" "$IMAGES_MD" "$COMPAT_MD"; do
  grep -qF "$NEW" "$f" \
    || { echo "::error::$f does not contain the new pin — pin line format drifted"; exit 1; }
done
echo "pinned chromium $NEW in $DOCKERFILE, $IMAGES_MD, $COMPAT_MD"
