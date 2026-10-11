#!/usr/bin/env bash
# failed-digest-cleanup.test.sh — SR-5-F3 pins. The publishing workflows
# push images by digest BEFORE the trivy gate; a gate-failed run must
# delete exactly the versions it pushed — and never a published
# release's. Asserts:
#
#   (a) every workflow with a pre-scan push path (images.yml,
#       release.yml, runtime-images.yml) has a `cleanup` job gated on
#       failure() + that workflow's publish condition, needing every
#       pre-publish leg — never promote/publish (signed content is out
#       of scope);
#   (b) the job consumes only THIS run's image-ref artifacts
#       (gh run download $GITHUB_RUN_ID, exact names) and bounds
#       deletions to this run's window (run_started_at from the API);
#   (c) cleanup-failed-digests.sh refuses WHOLE-TREE: any candidate
#       carrying a non-digest tag or reachable from a tagged manifest
#       cancels the image's deletion without failing the job;
#   (d) no wildcard/untagged sweep exists anywhere — the only DELETE
#       call is gated by run-tree membership + the run window;
#   plus unit tests of the helper against stubbed gh/curl: index ->
#   children resolution, referrers, sha256-* tag-derived versions,
#   shared child / shared index / compromised ref / cosign-tag
#   reverse-reference refusals, created_at windowing, pagination.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMG="$ROOT/.github/workflows/images.yml"
REL="$ROOT/.github/workflows/release.yml"
TRAIN="$ROOT/.github/workflows/runtime-images.yml"
HELPER="$ROOT/.github/scripts/cleanup-failed-digests.sh"
README="$ROOT/.github/README.md"
fails=0

chk() { # chk <desc> <file> <regex>
  local desc="$1" file="$2" pat="$3"
  if ! grep -qE -e "$pat" "$file"; then
    echo "FAIL: $desc"; fails=1
  fi
}
chk_absent() { # chk_absent <desc> <file> <regex>
  local desc="$1" file="$2" pat="$3"
  if grep -qE -e "$pat" "$file"; then
    echo "FAIL: $desc"; fails=1
  fi
}

job_block() { # job_block <file> <job-key> — the job's lines
  awk -v key="  $2:" '
    $0 ~ "^  [a-zA-Z0-9_-]+:" && $0 != key && started { exit }
    $0 == key { started=1 }
    started { print }
  ' "$1"
}

# ---------------------------------------------------------------
# (a) the cleanup job exists, gated on failure() + publish condition,
#     needing every pre-publish leg — never promote/publish.
# ---------------------------------------------------------------
chk "images.yml: cleanup job exists" "$IMG" '^  cleanup:$'
chk "release.yml: cleanup job exists" "$REL" '^  cleanup:$'
chk "runtime-images.yml: cleanup job exists" "$TRAIN" '^  cleanup:$'

IMGC="$(job_block "$IMG" cleanup)"
RELC="$(job_block "$REL" cleanup)"
TRAINC="$(job_block "$TRAIN" cleanup)"

grep -qF 'needs: [base, build, scan]' <<< "$IMGC" \
  || { echo "FAIL: images.yml cleanup needs [base, build, scan]"; fails=1; }
grep -qF 'needs: [prepare, base, images, scan, chart, binaries]' <<< "$RELC" \
  || { echo "FAIL: release.yml cleanup needs all pre-publish legs"; fails=1; }
grep -qF 'needs: [base, profile, scan]' <<< "$TRAINC" \
  || { echo "FAIL: runtime-images.yml cleanup needs [base, profile, scan]"; fails=1; }

for b in "$IMGC" "$TRAINC"; do
  grep -qE 'if:.*failure\(\)' <<< "$b" \
    || { echo "FAIL: cleanup job is not gated on failure()"; fails=1; }
  grep -qF "github.ref == 'refs/heads/main'" <<< "$b" \
    || { echo "FAIL: cleanup job is not gated on the main-ref publish condition"; fails=1; }
done
grep -qE 'if:.*failure\(\)' <<< "$RELC" \
  || { echo "FAIL: release.yml cleanup is not gated on failure()"; fails=1; }
grep -qF "needs.prepare.outputs.dry_run != 'true'" <<< "$RELC" \
  || { echo "FAIL: release.yml cleanup is not gated on dry_run != true"; fails=1; }

# never need the promote/publish job — its signed output is out of scope
for b in "$IMGC" "$RELC" "$TRAINC"; do
  grep -qE 'needs:.*(promote|publish)' <<< "$b" \
    && { echo "FAIL: cleanup job depends on a promote/publish job"; fails=1; }
done

# least privilege: packages: write scoped to the jobs that need it
for b in "$IMGC" "$RELC" "$TRAINC"; do
  grep -qF 'packages: write' <<< "$b" \
    || { echo "FAIL: cleanup job lacks packages: write"; fails=1; }
done

# ---------------------------------------------------------------
# (b) consumes only this run's digest outputs + the run window.
# ---------------------------------------------------------------
for b in "$IMGC" "$RELC" "$TRAINC"; do
  grep -qF 'gh run download "$GITHUB_RUN_ID"' <<< "$b" \
    || { echo "FAIL: cleanup does not fetch this run's artifacts"; fails=1; }
  grep -qF 'image-ref-' <<< "$b" \
    || { echo "FAIL: cleanup does not consume image-ref artifacts"; fails=1; }
  grep -qF 'actions/runs/$GITHUB_RUN_ID' <<< "$b" \
    || { echo "FAIL: cleanup does not resolve this run's started_at"; fails=1; }
  grep -qF 'RUN_STARTED_AT' <<< "$b" \
    || { echo "FAIL: cleanup does not bound deletions to the run window"; fails=1; }
  grep -qF 'cleanup-failed-digests.sh refs' <<< "$b" \
    || { echo "FAIL: cleanup does not invoke cleanup-failed-digests.sh refs"; fails=1; }
done

# ---------------------------------------------------------------
# (c)+(d) helper: whole-tree refusal; run-window bound; no sweep; the
# only DELETE call site is gated by targeting.
# ---------------------------------------------------------------
chk "helper exists" "$HELPER" 'cleanup-failed-digests'
chk "helper: digest-derived tag allowlist" "$HELPER" \
  'TAG_OK_RE=.?\^sha256-\[0-9a-f\]\{64\}'
chk "helper: refuses (whole-tree) on tagged/overlapping content" "$HELPER" \
  'refusing to delete'
chk "helper: run-window bound via RUN_STARTED_AT" "$HELPER" 'RUN_STARTED_AT'
chk "helper: created_at read from versions" "$HELPER" 'created_at'
chk "helper: tagged-manifest reachability set" "$HELPER" 'referenced\['
chk "helper: index -> children resolution" "$HELPER" '/manifests/'
chk "helper: referrers resolution" "$HELPER" '/referrers/'
chk "helper: run-digest tag derivation" "$HELPER" 'sha256-\$\{hex\}'

# one DELETE call site only, positioned after the targeting checks
n="$(grep -c 'X DELETE' "$HELPER")"
[ "${n:-0}" -eq 1 ] \
  || { echo "FAIL: helper has $n DELETE call sites (want exactly 1)"; fails=1; }
la="$(grep -n 'referenced\[' "$HELPER" | head -1 | cut -d: -f1)"
lb="$(grep -n 'X DELETE' "$HELPER" | head -1 | cut -d: -f1)"
{ [ -n "$la" ] && [ -n "$lb" ] && [ "$la" -lt "$lb" ]; } \
  || { echo "FAIL: reachability check does not precede the DELETE"; fails=1; }

# no sweep semantics anywhere outside comments
chk_absent "helper: no untagged/age/count sweep (code)" \
  <(grep -vE '^[[:space:]]*#' "$HELPER") 'untagged|keep-n'

# the frozen v1.0 review record is not edited by this change
chk_absent "review-v1.0.md untouched (frozen record)" \
  <(git diff origin/main -- docs/security/review-v1.0.md 2>/dev/null) 'SR-5-F3'

# ---------------------------------------------------------------
# unit tests — stubbed gh + curl fixtures.
# ---------------------------------------------------------------
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
SHIM="$WORK/bin"
mkdir -p "$SHIM" "$WORK/manifests" "$WORK/referrers" "$WORK/pkgs" "$WORK/refs"

cat > "$SHIM/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_LOG"
for a in "$@"; do url="$a"; done
if [[ "$*" == *"-X DELETE"* ]]; then
  exit "${STUB_DELETE_RC:-0}"
fi
if [ "${1:-}" = "api" ]; then
  pkg="$(sed -n 's|.*/container/\([^/]*\)/versions.*|\1|p' <<< "$url")"
  if [ -n "$pkg" ] && [ -f "$STUB_PKG_DIR/$pkg.json" ]; then
    cat "$STUB_PKG_DIR/$pkg.json"; exit 0
  fi
  echo "gh: HTTP 404 Not Found" >&2
  exit 1
fi
exit 1
EOF

cat > "$SHIM/curl" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do url="$a"; done
echo "curl $url" >> "$STUB_LOG"
case "$url" in
  *ghcr.io/token*) echo '{"token":"stub"}' ;;
  */manifests/*)
    d="${url##*/manifests/}"
    [ -f "$STUB_MANIFEST_DIR/$d.json" ] && cat "$STUB_MANIFEST_DIR/$d.json" \
      || { echo "curl: (22) 404" >&2; exit 22; } ;;
  */referrers/*)
    d="${url##*/referrers/}"
    [ -f "$STUB_REFERRER_DIR/$d.json" ] && cat "$STUB_REFERRER_DIR/$d.json" \
      || { echo "curl: (22) 404" >&2; exit 22; } ;;
  *) echo "curl: unstubbed $url" >&2; exit 1 ;;
esac
EOF
chmod +x "$SHIM/gh" "$SHIM/curl"

export STUB_LOG="$WORK/stub.log" STUB_PKG_DIR="$WORK/pkgs" \
       STUB_MANIFEST_DIR="$WORK/manifests" STUB_REFERRER_DIR="$WORK/referrers"
export PATH="$SHIM:$PATH" GH_TOKEN=test GITHUB_ACTOR=test \
       RUN_STARTED_AT="2026-10-11T12:00:00Z"

FRESH="2026-10-11T12:30:00Z"   # inside the run window
OLD="2026-10-10T00:00:00Z"     # before the run — a pre-existing version

run_case() { # run_case <desc> <want-rc>
  local desc="$1" want="$2" out rc
  : > "$STUB_LOG"
  out="$(bash "$HELPER" "$WORK/refs" 2>&1)"; rc=$?
  if [ "$rc" -ne "$want" ]; then
    echo "FAIL: $desc (rc=$rc, want $want)"; echo "$out" | sed 's/^/    /'
    fails=1
  else
    echo "ok: $desc"
  fi
  LAST_OUT="$out"
}

deletes_for_pkg() { # package -> sorted unique deleted version ids
  grep -oE "container/$1/versions/[0-9]+" "$STUB_LOG" | cut -d/ -f4 | sort -u
}
expect_deletes() { # expect_deletes <pkg> <space-separated ids incl trailing space>
  local got
  got="$(deletes_for_pkg "$1" | tr '\n' ' ')"
  [ "$got" = "$2" ] \
    || { echo "FAIL: $1 deleted ids '$got' want '$2'"; fails=1; }
}

mkref() { printf '%s' "$2" > "$WORK/refs/$1.ref"; }
ver_json() { # ver_json <id> <digest> <comma-tags> <created_at>
  local tags="" t
  local IFS=','
  for t in $3; do tags+="${tags:+,}\"$t\""; done
  printf '{"id": %s, "name": "%s", "created_at": "%s",
    "metadata": {"container": {"tags": [%s]}}}' \
    "$1" "$2" "$4" "$tags"
}
pkg_json() { local IFS=,; printf '[%s]\n' "$*"; }
idx_json() { # idx_json <digest...> -> OCI index listing children
  local m="" d
  for d in "$@"; do m+="${m:+,}{\"digest\":\"$d\"}"; done
  printf '{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[%s]}' "$m"
}

dg() { printf 'sha256:%s' "$(printf "$1%.0s" $(seq 32))"; } # dg <2hex> -> digest

IDX="$(dg aa)";   C1="$(dg bb)";   C2="$(dg cc)";   ATT="$(dg dd)"
REFR="$(dg ee)";  SIGM="$(dg f1)";  PUB="$(dg 99)";   TIDX="$(dg 88)"
SHARED="$(dg 77)"; SDG="$(dg 66)";  TDX="$(dg 55)"
IDXH="${IDX#sha256:}"; TDXH="${TDX#sha256:}"

# --- happy path: fresh tree, everything unreferenced --------------------
idx_json "$C1" "$C2" "$ATT" > "$STUB_MANIFEST_DIR/$IDX.json"
idx_json "$REFR"            > "$STUB_REFERRER_DIR/$IDX.json"
pkg_json \
  "$(ver_json 11 "$IDX" "" "$FRESH")" \
  "$(ver_json 12 "$C1" "" "$FRESH")" \
  "$(ver_json 13 "$C2" "" "$FRESH")" \
  "$(ver_json 14 "$ATT" "" "$FRESH")" \
  "$(ver_json 15 "$REFR" "" "$FRESH")" \
  "$(ver_json 16 "$SIGM" "sha256-$IDXH.sig" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"

mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"
run_case "fresh tree: index + children + referrers + sig deleted" 0
expect_deletes tinycdi-backend "11 12 13 14 15 16 "

# --- whole-tree refusal: shared child with a tagged index ---------------
# our fresh index lists [C1, SHARED]; a tagged index TIDX lists SHARED —
# deleting SHARED would break the published image. Mandatory advisor case.
idx_json "$C1" "$SHARED" > "$STUB_MANIFEST_DIR/$IDX.json"
idx_json "$SHARED"       > "$STUB_MANIFEST_DIR/$TIDX.json"
rm -f "$STUB_REFERRER_DIR/$IDX.json"
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 23 "$SHARED" "" "$FRESH")" \
  "$(ver_json 24 "$TIDX" "main" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "shared child with a published tagged index: whole-tree refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: shared-child refusal not logged"; fails=1; }
grep -q 'reachable from a tagged manifest' <<< "$LAST_OUT" \
  || { echo "FAIL: reachability reason not logged"; fails=1; }

# same case but paginated: the tagged version sits on "page 2" of the
# versions listing (gh api --paginate concatenates pages).
printf '[%s][%s]\n' \
  "$(ver_json 21 "$IDX" "" "$FRESH"),$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 23 "$SHARED" "" "$FRESH"),$(ver_json 24 "$TIDX" "main" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "shared child, tagged manifest on page 2: refusal holds" 0
expect_deletes tinycdi-backend ""

# --- compromised leg: ref points at a published tagged digest -----------
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$TIDX"
pkg_json \
  "$(ver_json 24 "$TIDX" "main" "$OLD")" \
  "$(ver_json 23 "$SHARED" "" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "ref pointing at a published digest deletes nothing" 0
expect_deletes tinycdi-backend ""
grep -q 'refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: published-digest refusal not logged"; fails=1; }

# --- reverse reference via cosign sig tag -------------------------------
# our fresh index lists child SDG; SDG carries tag sha256-<TDX>.sig and
# TDX is tagged -> SDG is published content (TDX's signature).
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"
idx_json "$C1" "$SDG" > "$STUB_MANIFEST_DIR/$IDX.json"
idx_json > "$STUB_MANIFEST_DIR/$TDX.json"   # TDX: leaf manifest, no children
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 25 "$SDG" "sha256-$TDXH.sig" "$FRESH")" \
  "$(ver_json 26 "$TDX" "main" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "cosign sig-tag reverse reference: whole-tree refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: sig-tag refusal not logged"; fails=1; }

# --- candidate carrying a real tag -> whole-tree refusal ----------------
pkg_json \
  "$(ver_json 21 "$IDX" "sha-deadbe" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
idx_json "$C1" > "$STUB_MANIFEST_DIR/$IDX.json"
run_case "tagged candidate refuses the whole image" 0
expect_deletes tinycdi-backend ""
grep -q 'non-digest tag' <<< "$LAST_OUT" \
  || { echo "FAIL: tagged-candidate refusal reason not logged"; fails=1; }

# --- created_at window: pre-existing member excluded, fresh members go --
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 25 "$SDG" "" "$OLD")" \
  "$(ver_json 27 "$(dg 44)" "" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
idx_json "$C1" "$SDG" > "$STUB_MANIFEST_DIR/$IDX.json"
run_case "pre-existing member excluded, fresh members deleted" 0
expect_deletes tinycdi-backend "21 22 "
grep -q 'skipping pre-existing version 25' <<< "$LAST_OUT" \
  || { echo "FAIL: pre-existing exclusion not logged"; fails=1; }

# identical rebuild dedup: the pushed index predates the run -> nothing
pkg_json \
  "$(ver_json 21 "$IDX" "" "$OLD")" \
  "$(ver_json 22 "$C1" "" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
idx_json "$C1" > "$STUB_MANIFEST_DIR/$IDX.json"
run_case "identical rebuild (pre-existing index) deletes nothing" 0
expect_deletes tinycdi-backend ""

# unrelated untagged versions are never swept
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 28 "$(dg 33)" "" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "unrelated untagged version is left alone" 0
expect_deletes tinycdi-backend "21 22 "

# --- non-publishing leg + malformed refs ---------------------------------
rm -f "$WORK/refs"/*
mkref backend local
run_case "'local' ref is skipped" 0
grep -q 'versions' "$STUB_LOG" \
  && { echo "FAIL: 'local' ref still hit the API"; fails=1; }

mkref backend "ghcr.io/evil/tinycdi-backend@$IDX"
run_case "foreign-namespace ref fails closed" 1
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend:latest"
run_case "tagged ref fails closed" 1

# version listing lag: pushed digest matches no version -> warning, rc 0
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"
echo "[]" > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "missing versions warn but pass" 0
grep -q 'matched no package version' <<< "$LAST_OUT" \
  || { echo "FAIL: missing-version warning absent"; fails=1; }

[ "$fails" -eq 0 ] && echo "failed-digest-cleanup: all guards pass"
exit "$fails"
