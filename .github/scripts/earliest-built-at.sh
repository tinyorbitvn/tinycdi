#!/usr/bin/env bash
# earliest-built-at.sh — print the earliest build time recorded for a set
# of images (R1-09). Each build job writes <image>.built-at next to its
# <image>.ref in the image-ref-<image> artifact: the same UTC second it
# stamps into the image's org.opencontainers.image.created label. The
# runtime train's manifest builtAt and a release chart's
# images.<key>.builtAt come from here, never from the clock at publish
# time, so a late approval or a re-run of an old run cannot make an old
# image look new.
#
# Usage: earliest-built-at.sh <refs-dir> <image> [<image>...]
# Prints YYYY-MM-DDTHH:MM:SSZ; exits 1 on a missing, malformed or
# impossible time (fail closed — the caller never signs without one).
set -euo pipefail

DIR="${1:?usage: earliest-built-at.sh <refs-dir> <image> [<image>...]}"
shift
[ "$#" -gt 0 ] || { echo "::error::no images given" >&2; exit 1; }

min=""
for img in "$@"; do
  f="$DIR/$img.built-at"
  [ -f "$f" ] || { echo "::error::missing build time '$f' for image '$img'" >&2; exit 1; }
  v="$(tr -d '[:space:]' < "$f")"
  [[ "$v" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] \
    || { echo "::error::$f is not a UTC RFC 3339 second ('$v')" >&2; exit 1; }
  # GNU date normalises nothing back: round-trip to reject 2026-09-31 etc.
  [ "$(date -u -d "$v" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)" = "$v" ] \
    || { echo "::error::$f is not a real date ('$v')" >&2; exit 1; }
  # Fixed-width UTC form: lexical order is time order.
  if [ -z "$min" ] || [[ "$v" < "$min" ]]; then min="$v"; fi
done
echo "$min"
