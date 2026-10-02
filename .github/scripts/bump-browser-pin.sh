#!/usr/bin/env bash
# bump-browser-pin.sh — repin one browser engine's apt pin in the browser
# image and its doc mirrors (D27). runtime-freshness.yml runs this once per
# engine that check-browser-freshness.sh reports as stale (newer available
# or pinned version gone), then opens the resulting diff as a PR.
#
# Engines and what is rewritten:
#   chromium     ARG CHROMIUM_APT_VERSION= in the Dockerfile; the
#                `- Chromium: `X`` pin line in docs/images.md; the
#                `current ARG `X`` line in docs/compatibility.md plus that
#                bullet's `as of` stamp (bump date)
#   firefox-esr  ARG FIREFOX_ESR_APT_VERSION= in the Dockerfile; the
#                `- Firefox ESR (fallback): `X`` pin line in
#                docs/images.md; the `| Firefox ESR fallback |` row in
#                docs/compatibility.md — version AND the locked deb sha256
#                (BUMP_DEB_SHA256, required: a version without its hash
#                would leave the locked-pin table lying)
# The "Known limitations" scan notes in docs/images.md keep the version
# they measured — they are historical, not a pin.
#
# Every extra file named after compatibility.md (NOTICE,
# THIRD_PARTY_LICENSES.md, …) lists the shipped versions; the old version
# is replaced there literally.
#
# Usage: bump-browser-pin.sh <engine> <new-version> [dockerfile] [images.md] [compatibility.md] [extra-file...]
# Env:   BUMP_DATE        — 'as of' stamp written to compatibility.md
#                           (default: today, UTC)
#        BUMP_DEB_SHA256  — sha256 of the new firefox-esr .deb (required
#                           for firefox-esr; 64 lowercase hex)
set -euo pipefail

USAGE="usage: bump-browser-pin.sh <chromium|firefox-esr> <new-version> [dockerfile] [images.md] [compatibility.md] [extra-file...]"
ENGINE="${1:-}"
NEW="${2:-}"
DOCKERFILE="${3:-build/browser/Dockerfile}"
IMAGES_MD="${4:-docs/images.md}"
COMPAT_MD="${5:-docs/compatibility.md}"
EXTRA_FILES=("${@:6}")
STAMP="${BUMP_DATE:-$(date -u +%F)}"
SHA="${BUMP_DEB_SHA256:-}"

[ -n "$ENGINE" ] && [ -n "$NEW" ] || { echo "$USAGE" >&2; exit 2; }

case "$ENGINE" in
  chromium)    ARG=CHROMIUM_APT_VERSION;     IMAGES_SEL='$0 ~ /^- Chromium:/' ;;
  firefox-esr) ARG=FIREFOX_ESR_APT_VERSION;  IMAGES_SEL='$0 ~ /^- Firefox ESR/' ;;
  *) echo "::error::unknown engine '$ENGINE' (want chromium or firefox-esr)"; exit 2 ;;
esac

# Debian-version charset only — the value lands in a Dockerfile ARG, in
# markdown and in a git branch name; anything outside this set is either
# malformed or an injection attempt (spaces, ';', quotes, '/', '&').
[[ "$NEW" =~ ^[0-9][A-Za-z0-9.+:~_-]*$ ]] \
  || { echo "::error::refusing malformed $ENGINE version '$NEW'"; exit 1; }
if [ "$ENGINE" = firefox-esr ]; then
  [[ "$SHA" =~ ^[0-9a-f]{64}$ ]] \
    || { echo "::error::firefox-esr bump needs BUMP_DEB_SHA256 (64 lowercase hex), got '$SHA'"; exit 1; }
fi

OLD="$(awk -F= -v arg="$ARG" '$1 == "ARG " arg {print $2; exit}' "$DOCKERFILE")"
[ -n "$OLD" ] \
  || { echo "::error::$ARG not found in $DOCKERFILE"; exit 1; }
if [ "$OLD" = "$NEW" ]; then
  echo "$ENGINE pin already at $NEW — no-op"
  exit 0
fi
echo "bumping $ENGINE pin $OLD -> $NEW"

# Literal substitution — the pins carry '.'/'~' and awk's index() avoids
# regex surprises; only pin-declaring lines are touched so historical scan
# notes keep their measured version. Bare matches the version with or
# without backticks (extra files / the compatibility.md table prose).
repin_line() { # repin_line <file> <awk line-selector> <stamp-as-of:yes|no> <backticked:yes|no>
  awk -v old="$OLD" -v new="$NEW" -v stamp="$STAMP" -v dostamp="$3" -v ticks="$4" '
    {
      if ('"$2"') {
        o = old; n = new
        if (ticks == "yes") { o = "`" old "`"; n = "`" new "`" }
        while ((i = index($0, o)) > 0)
          $0 = substr($0, 1, i - 1) n substr($0, i + length(o))
      }
      if (dostamp == "yes" && $0 ~ /bookworm-security as of [0-9]{4}-[0-9]{2}-[0-9]{2}/)
        sub(/as of [0-9]{4}-[0-9]{2}-[0-9]{2}/, "as of " stamp)
      print
    }' "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}

awk -v arg="$ARG" -v new="$NEW" '
  $0 ~ "^ARG " arg "=" { $0 = "ARG " arg "=" new }
  { print }' "$DOCKERFILE" > "$DOCKERFILE.tmp" && mv "$DOCKERFILE.tmp" "$DOCKERFILE"

repin_line "$IMAGES_MD" "$IMAGES_SEL" no yes
if [ "$ENGINE" = chromium ]; then
  repin_line "$COMPAT_MD" '$0 ~ /current ARG/' yes yes
else
  # The locked-pin table row: new version, new deb hash.
  repin_line "$COMPAT_MD" '$0 ~ /^\| Firefox ESR fallback \|/' no no
  awk -v sha="$SHA" '
    /^\| Firefox ESR fallback \|/ { sub(/deb sha256 `[0-9a-f]+`/, "deb sha256 `" sha "`") }
    { print }' "$COMPAT_MD" > "$COMPAT_MD.tmp" && mv "$COMPAT_MD.tmp" "$COMPAT_MD"
fi
for f in "${EXTRA_FILES[@]}"; do
  repin_line "$f" '$0 ~ /./' no no
done

# Fail loudly when a pin line drifts out of shape — better a red
# freshness job than a PR that bumps only some of the pins.
for f in "$DOCKERFILE" "$IMAGES_MD" "$COMPAT_MD" ${EXTRA_FILES[@]+"${EXTRA_FILES[@]}"}; do
  grep -qF "$NEW" "$f" \
    || { echo "::error::$f does not contain the new pin — pin line format drifted"; exit 1; }
done
if [ "$ENGINE" = firefox-esr ]; then
  grep -qF "deb sha256 \`$SHA\`" "$COMPAT_MD" \
    || { echo "::error::$COMPAT_MD does not carry the new deb sha256 — table row format drifted"; exit 1; }
fi
echo "pinned $ENGINE $NEW in $DOCKERFILE, $IMAGES_MD, $COMPAT_MD${EXTRA_FILES[*]:+, ${EXTRA_FILES[*]}}"
