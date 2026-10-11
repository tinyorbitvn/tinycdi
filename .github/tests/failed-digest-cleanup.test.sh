#!/usr/bin/env bash
# failed-digest-cleanup.test.sh — SR-5-F3 pins. The publishing workflows
# push images by digest BEFORE the trivy gate; a gate-failed run must
# delete exactly the versions it pushed. Asserts:
#
#   (a) every workflow with a pre-scan push path (images.yml,
#       release.yml, runtime-images.yml) has a `cleanup` job gated on
#       failure() + that workflow's publish condition, needing the push
#       and scan jobs — never promote/publish (signed content is out of
#       scope);
#   (b) the job consumes only THIS run's image-ref artifacts
#       (gh run download $GITHUB_RUN_ID, exact names);
#   (c) cleanup-failed-digests.sh refuses any version carrying a
#       non-digest tag (fail closed);
#   (d) no wildcard/untagged sweep exists anywhere — the only DELETE
#       call is gated by digest-set membership or a tag derived from
#       this run's index digest;
#   plus unit tests of the helper against stubbed gh/curl: index ->
#   children resolution, referrers, sha256-* tag-derived versions and
#   the tagged-version refusal.
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
#     needing push+scan jobs and never promote/publish.
# ---------------------------------------------------------------
chk "images.yml: cleanup job exists" "$IMG" '^  cleanup:$'
chk "release.yml: cleanup job exists" "$REL" '^  cleanup:$'
chk "runtime-images.yml: cleanup job exists" "$TRAIN" '^  cleanup:$'

IMGC="$(job_block "$IMG" cleanup)"
RELC="$(job_block "$REL" cleanup)"
TRAINC="$(job_block "$TRAIN" cleanup)"

grep -qF 'needs: [base, build, scan]' <<< "$IMGC" \
  || { echo "FAIL: images.yml cleanup needs [base, build, scan]"; fails=1; }
grep -qF 'needs: [prepare, base, images, scan]' <<< "$RELC" \
  || { echo "FAIL: release.yml cleanup needs [prepare, base, images, scan]"; fails=1; }
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

# least privilege: packages: write scoped to the jobs that need it —
# the cleanup job holds it; scan still holds only packages: read.
for b in "$IMGC" "$RELC" "$TRAINC"; do
  grep -qF 'packages: write' <<< "$b" \
    || { echo "FAIL: cleanup job lacks packages: write"; fails=1; }
done

# ---------------------------------------------------------------
# (b) consumes only this run's digest outputs — exact artifact names
#     from $GITHUB_RUN_ID, never a glob download, never another run.
# ---------------------------------------------------------------
for b in "$IMGC" "$RELC" "$TRAINC"; do
  grep -qF 'gh run download "$GITHUB_RUN_ID"' <<< "$b" \
    || { echo "FAIL: cleanup does not fetch this run's artifacts"; fails=1; }
  grep -qF 'image-ref-' <<< "$b" \
    || { echo "FAIL: cleanup does not consume image-ref artifacts"; fails=1; }
  grep -qF 'cleanup-failed-digests.sh refs' <<< "$b" \
    || { echo "FAIL: cleanup does not invoke cleanup-failed-digests.sh refs"; fails=1; }
done

# ---------------------------------------------------------------
# (c)+(d) helper: tagged-version refusal; no sweep semantics; the only
# DELETE call site is digest-gated.
# ---------------------------------------------------------------
chk "helper exists" "$HELPER" 'cleanup-failed-digests'
chk "helper: digest-derived tag allowlist" "$HELPER" \
  'TAG_OK_RE=.?\^sha256-\[0-9a-f\]\{64\}'
chk "helper: refuses non-digest tags" "$HELPER" 'refusing to delete'
chk "helper: index -> children resolution" "$HELPER" '/manifests/'
chk "helper: referrers resolution" "$HELPER" '/referrers/'
chk "helper: run-digest tag derivation" "$HELPER" 'sha256-\$\{hex\}'

# one DELETE call site only, positioned after the targeting checks
n="$(grep -c 'X DELETE' "$HELPER")"
[ "${n:-0}" -eq 1 ] \
  || { echo "FAIL: helper has $n DELETE call sites (want exactly 1)"; fails=1; }
la="$(grep -n 'target=1' "$HELPER" | head -1 | cut -d: -f1)"
lb="$(grep -n 'X DELETE' "$HELPER" | head -1 | cut -d: -f1)"
{ [ -n "$la" ] && [ -n "$lb" ] && [ "$la" -lt "$lb" ]; } \
  || { echo "FAIL: digest-set targeting does not precede the DELETE"; fails=1; }

# no sweep semantics anywhere outside comments
chk_absent "helper: no untagged/age/count sweep (code)" \
  <(grep -vE '^[[:space:]]*#' "$HELPER") 'untagged|keep-n'

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
export PATH="$SHIM:$PATH" GH_TOKEN=test GITHUB_ACTOR=test

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

mkref() { printf '%s' "$2" > "$WORK/refs/$1.ref"; }
ver_json() { # ver_json <id> <digest> <comma-tags>
  local tags="" t
  local IFS=','
  for t in $3; do tags+="${tags:+,}\"$t\""; done
  printf '{"id": %s, "name": "%s", "metadata": {"container": {"tags": [%s]}}}' \
    "$1" "$2" "$tags"
}
pkg_json() { local IFS=,; printf '[%s]\n' "$*"; }

IDX="sha256:$(printf 'aa%.0s' $(seq 32))"
C1="sha256:$(printf 'bb%.0s' $(seq 32))"
C2="sha256:$(printf 'cc%.0s' $(seq 32))"
ATT="sha256:$(printf 'dd%.0s' $(seq 32))"
REFR="sha256:$(printf 'ee%.0s' $(seq 32))"
SIGM="sha256:$(printf 'f1%.0s' $(seq 32))"
PUB="sha256:$(printf '99%.0s' $(seq 32))"
IDXH="${IDX#sha256:}"

# multi-arch index: two platform children + a buildkit attestation child
cat > "$STUB_MANIFEST_DIR/$IDX.json" <<EOF
{"mediaType":"application/vnd.oci.image.index.v1+json",
 "manifests":[{"digest":"$C1"},{"digest":"$C2"},{"digest":"$ATT"}]}
EOF
cat > "$STUB_REFERRER_DIR/$IDX.json" <<EOF
{"mediaType":"application/vnd.oci.image.index.v1+json",
 "manifests":[{"digest":"$REFR"}]}
EOF
pkg_json \
  "$(ver_json 11 "$IDX" "")" \
  "$(ver_json 12 "$C1" "")" \
  "$(ver_json 13 "$C2" "")" \
  "$(ver_json 14 "$ATT" "")" \
  "$(ver_json 15 "$REFR" "")" \
  "$(ver_json 16 "$SIGM" "sha256-$IDXH.sig")" \
  "$(ver_json 17 "$PUB" "sha256-$IDXH.sig,main")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"

mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"

# index + children + referrer + sha256-<idx>.sig version deleted; the
# version carrying `main` (17) is refused and the run fails closed.
run_case "index -> children + referrers + sig tag; tagged version refused" 1
got="$(deletes_for_pkg tinycdi-backend | tr '\n' ' ')"
want="11 12 13 14 15 16 "
[ "$got" = "$want" ] \
  || { echo "FAIL: deleted ids '$got' want '$want' (17 refused, nothing else)"; fails=1; }
grep -q 'carries non-digest tag' <<< "$LAST_OUT" \
  || { echo "FAIL: refusal reason not logged"; fails=1; }
grep -q 'main' <<< "$LAST_OUT" \
  || { echo "FAIL: refused tag name not logged"; fails=1; }

# an unrelated untagged version of the same package is never swept
pkg_json \
  "$(ver_json 11 "$IDX" "")" \
  "$(ver_json 12 "$C1" "")" \
  "$(ver_json 13 "$C2" "")" \
  "$(ver_json 14 "$ATT" "")" \
  "$(ver_json 15 "$REFR" "")" \
  "$(ver_json 16 "$SIGM" "sha256-$IDXH.sig")" \
  "$(ver_json 18 "sha256:$(printf '77%.0s' $(seq 32))" "")" \
  > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "unrelated untagged version is left alone" 0
got="$(deletes_for_pkg tinycdi-backend | tr '\n' ' ')"
want="11 12 13 14 15 16 "
[ "$got" = "$want" ] \
  || { echo "FAIL: sweep check — deleted '$got' want '$want'"; fails=1; }

# non-publishing leg + malformed refs
mkref backend local
run_case "'local' ref is skipped" 0
grep -q 'versions' "$STUB_LOG" \
  && { echo "FAIL: 'local' ref still hit the API"; fails=1; }

mkref backend "ghcr.io/evil/tinycdi-backend@$IDX"
run_case "foreign-namespace ref fails closed" 1
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend:latest"
run_case "tagged ref fails closed" 1

# version listing lag: pushed digest matches no version -> warning, rc 0
cat > "$STUB_MANIFEST_DIR/$IDX.json" <<EOF
{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}
EOF
mkref backend "ghcr.io/tinyorbitvn/tinycdi-backend@$IDX"
echo "[]" > "$STUB_PKG_DIR/tinycdi-backend.json"
run_case "missing versions warn but pass" 0
grep -q 'matched no package version' <<< "$LAST_OUT" \
  || { echo "FAIL: missing-version warning absent"; fails=1; }

[ "$fails" -eq 0 ] && echo "failed-digest-cleanup: all guards pass"
exit "$fails"
