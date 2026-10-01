#!/usr/bin/env bash
# stamp-image-digests.sh — stamp release image digests into chart values.
#
# SEC-18: the released chart must reference the exact image digests the
# pipeline built and scanned (digest wins over tag in the chart's image
# template), not mutable tags. Run before `helm package`.
#
# Usage: stamp-image-digests.sh <values.yaml> <refs-dir>
#   <refs-dir> holds one <image>.ref file per image written by the build
#   jobs; each file contains "<repo>@sha256:<digest>" or the literal
#   "local" for unpushed rehearsal builds (skipped).
set -euo pipefail

VALUES="$1"
REFS_DIR="$2"

# image name -> "<values.yaml section> <key>" — the digest lands at
# <section>.<key>.digest (a 2-space key under a top-level section, 4-space
# digest field). images.* for the component/runtime images; the kasm
# adapter lives at kasmAdapter.image.
path_for() {
  case "$1" in
    linux-desktop) echo "images linuxDesktop" ;;
    kasm-adapter)  echo "kasmAdapter image" ;;
    *)             echo "images $1" ;;
  esac
}

for f in "$REFS_DIR"/*.ref; do
  name="$(basename "$f" .ref)"
  ref="$(tr -d '[:space:]' < "$f")"
  [ "$ref" = "local" ] && continue
  digest="${ref##*@}"
  read -r section key <<< "$(path_for "$name")"
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "::error::ref file $f does not contain a sha256 digest ('$ref')"
    exit 1
  fi
  if ! awk -v section="$section" -v key="$key" -v digest="$digest" '
    /^[^ #]/              { in_sec = ($0 ~ ("^" section ":")) }
    in_sec && /^  [a-zA-Z]+:/ { cur = substr($1, 1, length($1) - 1) }
    in_sec && cur == key && /^    digest: / {
      sub(/digest:.*/, "digest: \"" digest "\""); stamped++
    }
    { print }
    END { if (stamped == 0) exit 42 }
  ' "$VALUES" > "$VALUES.stamped"; then
    rm -f "$VALUES.stamped"
    echo "::error::no $section.$key.digest field found in $VALUES"
    exit 1
  fi
  mv "$VALUES.stamped" "$VALUES"
  echo "stamped $section.$key.digest = $digest"
done
