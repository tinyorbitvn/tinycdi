#!/usr/bin/env bash
# threat-model-refs.test.sh — the threat register (docs/security/threat-model.md
# §6) has rows TM-01..TM-N without gaps, each with a Control and a Pinned by
# cell; every Go test the document names exists; every TM-NN cited anywhere
# resolves to a row; and no pre-rebuild item id (S<n>, A6-S<n>) is cited
# anywhere (they were renumbered to TM-NN).
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TM="$ROOT/docs/security/threat-model.md"
fails=0
mapfile -t ids < <(grep -oE '^\| TM-[0-9]{2} \|' "$TM" | grep -oE 'TM-[0-9]{2}')
[ "${#ids[@]}" -gt 0 ] || { echo "FAIL: no register rows (| TM-NN | …) in $TM"; exit 1; }
i=1
for id in "${ids[@]}"; do
  want="$(printf 'TM-%02d' "$i")"
  [ "$id" = "$want" ] || { echo "FAIL: register row $id out of order (want $want)"; fails=1; }
  i=$((i + 1))
done
# Columns: | ID | Boundary | Threat | Control | Pinned by | Residual |
# (a literal pipe inside a cell is written \| and is masked first).
while IFS= read -r bad; do
  echo "FAIL: register row without a Control or Pinned by cell: $bad"; fails=1
done < <(grep -E '^\| TM-[0-9]{2} \|' "$TM" | sed 's/\\|/¦/g' \
  | awk -F'|' '{c=$5; p=$6; gsub(/[ \t]/,"",c); gsub(/[ \t]/,"",p);
               if (NF < 8 || c == "" || c == "—" || p == "" || p == "—") print $2}')
while IFS= read -r t; do
  git -C "$ROOT" grep -qE "^func $t\(" -- '*.go' || { echo "FAIL: threat model cites $t, which no Go file defines"; fails=1; }
done < <(grep -oE '`Test[A-Za-z0-9_]+`' "$TM" | tr -d '`' | sort -u)
while IFS= read -r ref; do
  printf '%s\n' "${ids[@]}" | grep -qx "$ref" || { echo "FAIL: '$ref' is cited but has no register row"; fails=1; }
done < <(git -C "$ROOT" grep -ohE '\bTM-[0-9]{2}\b' -- ':(exclude,glob)docs/superpowers/**' | sort -u)
# Old item ids: S<n> or A6-S<n>, not preceded by a letter+slash (NFS/S3) or
# another word char or dash (FX-S1), not followed by a word char or dash
# (S3-fuse), by /NFS-style words (S3/NFS), or by an object-storage noun
# (S3 bucket). go.sum and lockfiles are excluded (base64 such as "+S0=").
old="$(git -C "$ROOT" grep -nIP '(?<![A-Za-z]/)(?<![\w-])(?:A6-)?S[0-9]{1,2}(?![\w-])(?!/[A-Z]{2,})(?! (?:bucket|buckets|storage|object|API|compatible)\b)' -- \
  ':(exclude,glob)docs/superpowers/**' ':(exclude).github/tests/threat-model-refs.test.sh' \
  ':(exclude)go.sum' ':(exclude,glob)**/package-lock.json' | head -20)"
[ -z "$old" ] || { echo "FAIL: pre-rebuild threat-model ids still cited:"; echo "$old"; fails=1; }
[ "$fails" -eq 0 ] && echo "threat-model-refs: ${#ids[@]} rows, every TM reference resolves, no S<n> ids left"
exit "$fails"
