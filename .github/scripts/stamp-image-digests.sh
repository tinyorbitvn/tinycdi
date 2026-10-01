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

# image name -> values.yaml key under images:
key_for() {
  case "$1" in
    linux-desktop) echo linuxDesktop ;;
    *)             echo "$1" ;;
  esac
}

for f in "$REFS_DIR"/*.ref; do
  name="$(basename "$f" .ref)"
  ref="$(tr -d '[:space:]' < "$f")"
  [ "$ref" = "local" ] && continue
  digest="${ref##*@}"
  key="$(key_for "$name")"
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "::error::ref file $f does not contain a sha256 digest ('$ref')"
    exit 1
  fi
  if ! awk -v key="$key" -v digest="$digest" '
    /^[^ #]/              { in_images = ($0 ~ /^images:/) }
    in_images && /^  [a-zA-Z]+:/ { cur = substr($1, 1, length($1) - 1) }
    in_images && cur == key && /^    digest: / {
      sub(/digest:.*/, "digest: \"" digest "\""); stamped++
    }
    { print }
    END { if (stamped == 0) exit 42 }
  ' "$VALUES" > "$VALUES.stamped"; then
    rm -f "$VALUES.stamped"
    echo "::error::no images.$key.digest field found in $VALUES"
    exit 1
  fi
  mv "$VALUES.stamped" "$VALUES"
  echo "stamped images.$key.digest = $digest"
done
