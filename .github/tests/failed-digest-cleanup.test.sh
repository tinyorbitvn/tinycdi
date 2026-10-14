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
#   plus the rehearsal input pins (images.yml only, dispatch+main-only,
#   default false, after the real scan + report uploads) and unit tests
#   of the helper against stubbed gh/curl: index -> children
#   resolution, referrers, sha256-* tag-derived versions, shared child /
#   shared index / compromised ref / cosign-tag reverse-reference
#   refusals, created_at windowing, pagination.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMG="$ROOT/.github/workflows/images.yml"
REL="$ROOT/.github/workflows/release.yml"
TRAIN="$ROOT/.github/workflows/runtime-images.yml"
HELPER="$ROOT/.github/scripts/cleanup-failed-digests.sh"
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
chk "helper: namespaced GHCR pull scope" "$HELPER" \
  'scope=repository:\$\(repo_path'
chk "helper: single repository-path helper" "$HELPER" 'repo_path()'
chk "helper: referrers fetch fails closed" "$HELPER" \
  'referrers fetch failed — refusing to delete'
chk "helper: referrers 404 means no referrers" "$HELPER" \
  '404) return 0'
chk "helper: referrers status captured via -w" "$HELPER" '%{http_code}'
chk "helper: referrers body validated as index" "$HELPER" \
  'manifests \| type'
chk_absent "helper: no un-namespaced /v2/ path" \
  <(grep -vE '^[[:space:]]*#' "$HELPER") 'v2/\$1/'
chk "helper: bounded recursive manifest walk" "$HELPER" 'tree_digests'
chk "helper: nested-index depth bound" "$HELPER" 'MAX_DEPTH'
chk "helper: unenumerable tagged manifest fails closed" "$HELPER" \
  'cannot enumerate tagged manifest'
chk "helper: versions-list failure is an error path" "$HELPER" \
  'cannot list package versions'
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

# cancel is out of coverage — the docs must not claim aborted/cancelled
# RUNS are cleaned. The pattern is deliberately narrow: it needs an
# abort/cancel word immediately followed by run/workflow/job (or the
# legacy 'or aborted' phrasing) — unrelated uses like "the handshake is
# aborted" must pass.
ABORT_RE='(abort|cancel)[a-z]* (run|workflow|job)|or aborted'
for doc in "$ROOT"/docs/releases/*.md "$ROOT/docs/security/provenance.md"; do
  [ -f "$doc" ] || continue
  grep -qiE "$ABORT_RE" "$doc" \
    && { echo "FAIL: $doc claims aborted/cancelled runs are cleaned"; fails=1; }
done
# fixtures: the old wording is still caught; an unrelated use passes.
grep -qiE "$ABORT_RE" <<< 'a gate-failed (or aborted) publish run used to leave' \
  || { echo "FAIL: abort pattern misses the old '(or aborted)' wording"; fails=1; }
grep -qiE "$ABORT_RE" <<< 'the TLS handshake is aborted after one retry' \
  && { echo "FAIL: abort pattern matches an unrelated sentence"; fails=1; }

# ---------------------------------------------------------------
# rehearsal input (images.yml only): a main-only, dispatch-only,
# default-off knob that forces the gate red AFTER the real scan and
# all report uploads — proving the cleanup end-to-end.
# ---------------------------------------------------------------
chk "images.yml: fail_scan_for_rehearsal input exists" "$IMG" \
  'fail_scan_for_rehearsal:'
chk "images.yml: rehearsal input is a boolean" "$IMG" 'type: boolean'
chk_absent "release.yml: no rehearsal input" "$REL" 'fail_scan_for_rehearsal'
chk_absent "runtime-images.yml: no rehearsal input" "$TRAIN" \
  'fail_scan_for_rehearsal'

SCANB="$(job_block "$IMG" scan)"
grep -qF 'fail_scan_for_rehearsal' <<< "$SCANB" \
  || { echo "FAIL: scan job lacks the rehearsal step"; fails=1; }
for pat in "github.event_name == 'workflow_dispatch'" \
           "github.ref == 'refs/heads/main'" "always()"; do
  grep -qF "$pat" <<< "$SCANB" \
    || { echo "FAIL: rehearsal step missing guard: $pat"; fails=1; }
done

# default false on the input declaration
awk '/fail_scan_for_rehearsal:/{f=1} f&&/default: false/{d=1}
     END{exit !d}' "$IMG" \
  || { echo "FAIL: rehearsal input does not default to false"; fails=1; }

# ordering: the rehearsal step sits after the real gate AND both
# report uploads, inside the scan job (before `promote:`)
scan_lines="$(awk '/^  scan:/{f=1} /^  promote:/{f=0} f' "$IMG")"
l_gate=$(grep -n 'trivy scan + gate' <<< "$scan_lines" | cut -d: -f1)
l_rep=$(grep -n 'upload trivy reports' <<< "$scan_lines" | cut -d: -f1)
l_sarif=$(grep -n 'upload SARIF to code scanning' <<< "$scan_lines" | cut -d: -f1)
l_reh=$(grep -n 'rehearsal — force the gate red' <<< "$scan_lines" | cut -d: -f1)
{ [ -n "$l_gate" ] && [ -n "$l_rep" ] && [ -n "$l_sarif" ] && [ -n "$l_reh" ] \
  && [ "$l_gate" -lt "$l_reh" ] && [ "$l_rep" -lt "$l_reh" ] \
  && [ "$l_sarif" -lt "$l_reh" ]; } \
  || { echo "FAIL: rehearsal step is not after the real gate + uploads"; fails=1; }

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
# The token endpoint REJECTS an un-namespaced repository scope — a bug
# this stub exists to catch (the real ghcr returns 400; a token from an
# un-namespaced scope 403s every namespaced GET). Manifest/referrer
# fetches must present the token issued for the namespaced scope.
for a in "$@"; do
  url="$a"
  case "$a" in
    "Authorization: Bearer "*) auth="${a#Authorization: Bearer }" ;;
  esac
done
echo "curl $url" >> "$STUB_LOG"
case "$url" in
  *ghcr.io/token*)
    # namespaced READ-ONLY scope only — a push/other-scope change
    # must fail here so the helper can never silently widen rights
    case "$url" in
      *"scope=repository:tinyorbitvn/"*":pull") echo '{"token":"ns-scoped"}' ;;
      *) echo 'curl: (22) 400 invalid scope' >&2; exit 22 ;;
    esac ;;
  */manifests/*|*/referrers/*)
    [ "${auth:-}" = "ns-scoped" ] \
      || { echo "curl: (22) 403 not the namespaced token" >&2; exit 22; }
    # GHCR answers a bare repository path with 400 NAME_INVALID —
    # model it so un-namespaced /v2/ code fails instead of passing on
    # fixtures the real registry would never serve. Fixtures are keyed
    # by the FULL repository path (<org>/<pkg>/<digest>).
    p="${url#*ghcr.io/v2/}"
    kind=manifests; base="${p%%/manifests/*}"
    case "$p" in */referrers/*) kind=referrers; base="${p%%/referrers/*}" ;; esac
    d="${p##*/}"
    case "$base" in
      */*) ;;
      *) echo 'curl: (22) 400 NAME_INVALID' >&2; exit 22 ;;
    esac
    dir="$STUB_MANIFEST_DIR"; [ "$kind" = referrers ] && dir="$STUB_REFERRER_DIR"
    [ -f "$dir/$base/$d.error" ] && { echo 'curl: (22) 500' >&2; exit 22; }
    if [ "$kind" = referrers ]; then
      # model GHCR referrers semantics: a referrer-less subject answers
      # 404 MANIFEST_UNKNOWN (not 200 + empty index); status travels in
      # the -w tail line when the caller asks for it, or via exit 22
      # when the caller passes -f. .code forces a status, .neterr a
      # transport failure.
      has_f=0; has_w=0
      for a in "$@"; do
        case "$a" in -*f*) has_f=1 ;; -w|-w*) has_w=1 ;; esac
      done
      [ -f "$dir/$base/$d.neterr" ] \
        && { echo 'curl: (6) could not resolve host' >&2; exit 6; }
      code=404
      body='{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}'
      [ -f "$dir/$base/$d.code" ] \
        && { code="$(cat "$dir/$base/$d.code")"; body='{}'; }
      [ -f "$dir/$base/$d.json" ] \
        && { code=200; body="$(cat "$dir/$base/$d.json")"; }
      if [ "$has_f" = 1 ] && [ "$code" != 200 ]; then
        echo "curl: (22) $code" >&2; exit 22
      fi
      printf '%s\n' "$body"
      [ "$has_w" = 1 ] && printf '%s\n' "$code"
      exit 0
    fi
    [ -f "$dir/$base/$d.json" ] && { cat "$dir/$base/$d.json"; exit 0; }
    echo 'curl: (22) 404' >&2; exit 22 ;;
  *) echo "curl: unstubbed $url" >&2; exit 1 ;;
esac
EOF
chmod +x "$SHIM/gh" "$SHIM/curl"

export STUB_LOG="$WORK/stub.log" STUB_PKG_DIR="$WORK/pkgs" \
       STUB_MANIFEST_DIR="$WORK/manifests" STUB_REFERRER_DIR="$WORK/referrers"
export PATH="$SHIM:$PATH" GH_TOKEN=test GITHUB_ACTOR=test \
       RUN_STARTED_AT="2026-10-11T12:00:00Z"

# fixture keys are the FULL repository path — the stub 400s bare
# paths like the real registry does
MDIR="$STUB_MANIFEST_DIR/tinyorbitvn/tinycdi-backend"
RDIR="$STUB_REFERRER_DIR/tinyorbitvn/tinycdi-backend"
mkdir -p "$MDIR" "$RDIR"

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
idx_json() { # idx_json <child>... — "sha256:..." leaf manifest or
  # "i:sha256:..." a nested index child (recursed into by tree_digests)
  local m="" d
  for d in "$@"; do
    case "$d" in
      i:*) m+="${m:+,}{\"digest\":\"${d#i:}\",\"mediaType\":\"application/vnd.oci.image.index.v1+json\"}" ;;
      *)   m+="${m:+,}{\"digest\":\"$d\",\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\"}" ;;
    esac
  done
  printf '{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[%s]}' "$m"
}

dg() { printf 'sha256:%s' "$(printf "$1%.0s" $(seq 32))"; } # dg <2hex> -> digest

IDX="$(dg aa)";   C1="$(dg bb)";   C2="$(dg cc)";   ATT="$(dg dd)"
REFR="$(dg ee)";  SIGM="$(dg f1)";  TIDX="$(dg 88)"
SHARED="$(dg 77)"; SDG="$(dg 66)";  TDX="$(dg 55)"
NIDX="$(dg 22)";  GRAND="$(dg 11)"
N1="$(dg a1)"; N2="$(dg a2)"; N3="$(dg a3)"; N4="$(dg a4)"; N5="$(dg a5)"
IDXH="${IDX#sha256:}"; TDXH="${TDX#sha256:}"

# --- happy path: fresh tree, everything unreferenced --------------------
idx_json "$C1" "$C2" "$ATT" > "$MDIR/$IDX.json"
idx_json "$REFR"            > "$RDIR/$IDX.json"
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
idx_json "$C1" "$SHARED" > "$MDIR/$IDX.json"
idx_json "$SHARED"       > "$MDIR/$TIDX.json"
rm -f "$RDIR/$IDX.json"
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
idx_json "$C1" "$SDG" > "$MDIR/$IDX.json"
idx_json > "$MDIR/$TDX.json"   # TDX: leaf manifest, no children
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

# --- nested index inside our tree: grandchild resolved + deleted --------
idx_json "$C1" "i:$NIDX" > "$MDIR/$IDX.json"
idx_json "$GRAND"        > "$MDIR/$NIDX.json"
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 23 "$NIDX" "" "$FRESH")" \
  "$(ver_json 24 "$GRAND" "" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"
run_case "nested index inside our tree: grandchild resolved + deleted" 0
expect_deletes tinycdi-backend "21 22 23 24 "

# --- nested index protecting a grandchild of a tagged manifest ----------
# TIDX (tagged main) -> NIDX -> SHARED; our fresh index lists SHARED:
# reachability must follow the nested index and refuse the whole image.
idx_json "$C1" "$SHARED" > "$MDIR/$IDX.json"
idx_json "i:$NIDX"       > "$MDIR/$TIDX.json"
idx_json "$SHARED"       > "$MDIR/$NIDX.json"
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 23 "$SHARED" "" "$FRESH")" \
  "$(ver_json 25 "$TIDX" "main" "$OLD")" \
  "$(ver_json 26 "$NIDX" "" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "grandchild behind a tagged nested index: whole-tree refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'reachable from a tagged manifest' <<< "$LAST_OUT" \
  || { echo "FAIL: nested reachability reason not logged"; fails=1; }

# --- an index nested deeper than the bound refuses the whole image ------
idx_json "i:$N1" > "$MDIR/$IDX.json"
idx_json "i:$N2" > "$MDIR/$N1.json"
idx_json "i:$N3" > "$MDIR/$N2.json"
idx_json "i:$N4" > "$MDIR/$N3.json"
idx_json "i:$N5" > "$MDIR/$N4.json"   # index child at depth 5
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "index nested deeper than 4: refuse whole image" 0
expect_deletes tinycdi-backend ""
grep -q 'nested deeper than' <<< "$LAST_OUT" \
  || { echo "FAIL: depth-bound refusal not logged"; fails=1; }

# --- cannot enumerate a tagged manifest: refuse, delete nothing ---------
rm -f "$MDIR/$TIDX.json"   # tagged manifest is unreadable
idx_json "$C1" > "$MDIR/$IDX.json"
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  "$(ver_json 25 "$TIDX" "main" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "unenumerable tagged manifest: refuse, delete nothing" 0
expect_deletes tinycdi-backend ""
grep -q 'cannot enumerate tagged manifest' <<< "$LAST_OUT" \
  || { echo "FAIL: unenumerable-manifest refusal not logged"; fails=1; }

# --- referrers fetch error: fail-closed refusal, nothing deleted --------
# a failed referrers GET must NOT read as "no referrers" — the tree is
# possibly incomplete, so the whole image is refused (rc 0, logged)
idx_json "$C1" > "$MDIR/$IDX.json"
echo "500" > "$RDIR/$IDX.code"
pkg_json \
  "$(ver_json 21 "$IDX" "" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"
run_case "referrers 5xx: fail-closed refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'referrers fetch failed — refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: referrers-error refusal not logged"; fails=1; }
rm -f "$RDIR/$IDX.code"

# --- every other referrers error class must also fail closed ------------
for st in 401 403 400 503; do
  echo "$st" > "$RDIR/$IDX.code"
  run_case "referrers HTTP $st: fail-closed refusal" 0
  expect_deletes tinycdi-backend ""
  grep -q 'referrers fetch failed — refusing to delete' <<< "$LAST_OUT" \
    || { echo "FAIL: referrers $st refusal not logged"; fails=1; }
  rm -f "$RDIR/$IDX.code"
done
touch "$RDIR/$IDX.neterr"
run_case "referrers network error: fail-closed refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'referrers fetch failed — refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: referrers network-error refusal not logged"; fails=1; }
rm -f "$RDIR/$IDX.neterr"

# --- a 200 body that is not a manifest index must fail closed -----------
# unparseable JSON, or parseable-but-not-an-index (e.g. an error doc):
# neither proves "no referrers"
echo 'this-is-not-json' > "$RDIR/$IDX.json"
run_case "referrers malformed body: fail-closed refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'referrers fetch failed — refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: malformed-body refusal not logged"; fails=1; }
echo '{"errors":[{"code":"BLOB_UNKNOWN","message":"x"}]}' > "$RDIR/$IDX.json"
run_case "referrers non-index body: fail-closed refusal" 0
expect_deletes tinycdi-backend ""
grep -q 'referrers fetch failed — refusing to delete' <<< "$LAST_OUT" \
  || { echo "FAIL: non-index-body refusal not logged"; fails=1; }
rm -f "$RDIR/$IDX.json"

# --- referrers 404 = no referrers: the tree deletes normally ------------
# GHCR answers 404 MANIFEST_UNKNOWN for a referrer-less subject (the
# default stub answer with no fixture present); the subject's
# existence is already proven by the manifest fetch, so 404 is
# "empty", not "unknown" — the same case under the OLD code passes
# only because '|| true' also hid the error, which is why the error
# classes above carry the weight.
run_case "referrers 404 = empty list: tree deletes" 0
expect_deletes tinycdi-backend "21 22 "

# --- versions-list API error: nothing deleted, job fails ----------------
rm -f "$STUB_PKG_DIR/tinycdi-backend.json"   # stub gh exits nonzero
run_case "versions listing API error: delete nothing" 1
expect_deletes tinycdi-backend ""
grep -q 'cannot list package versions' <<< "$LAST_OUT" \
  || { echo "FAIL: versions-list error not logged"; fails=1; }

# --- candidate carrying a real tag -> whole-tree refusal ----------------
pkg_json \
  "$(ver_json 21 "$IDX" "sha-deadbe" "$FRESH")" \
  "$(ver_json 22 "$C1" "" "$FRESH")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
idx_json "$C1" > "$MDIR/$IDX.json"
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
idx_json "$C1" "$SDG" > "$MDIR/$IDX.json"
run_case "pre-existing member excluded, fresh members deleted" 0
expect_deletes tinycdi-backend "21 22 "
grep -q 'skipping pre-existing version 25' <<< "$LAST_OUT" \
  || { echo "FAIL: pre-existing exclusion not logged"; fails=1; }

# identical rebuild dedup: the pushed index predates the run -> nothing
pkg_json \
  "$(ver_json 21 "$IDX" "" "$OLD")" \
  "$(ver_json 22 "$C1" "" "$OLD")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
idx_json "$C1" > "$MDIR/$IDX.json"
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
