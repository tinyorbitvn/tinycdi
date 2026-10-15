#!/usr/bin/env bash
# push-sign-chart.test.sh — R1-40 / SUPR-13: the release chart is signed
# right after `helm push`, and any failure once the push has started
# deletes exactly the GHCR version this run created, so no unsigned
# chart stays tagged. A version that predates the run is never deleted.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/.github/scripts/push-sign-chart.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
mkdir -p "$D/bin"; LOG="$D/calls.log"
fails=0
DG="sha256:$(printf 'c%.0s' $(seq 64))"

cat > "$D/bin/helm" <<'SH'
#!/usr/bin/env bash
echo "helm $*" >> "$STUB_LOG"
[ -n "${STUB_HELM_FAIL:-}" ] && { echo "Error: push failed" >&2; exit 1; }
echo "Pushed: ghcr.io/test/charts/tinycdi:1.0.1"
SH
cat > "$D/bin/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >> "$STUB_LOG"
[ -n "${STUB_NO_DIGEST:-}" ] && exit 1
printf 'Name:      %s\nMediaType: application/vnd.oci.image.manifest.v1+json\nDigest:    %s\n' "${@: -1}" "$STUB_DIGEST"
SH
cat > "$D/bin/cosign" <<'SH'
#!/usr/bin/env bash
echo "cosign $*" >> "$STUB_LOG"
[ -n "${STUB_COSIGN_FAIL:-}" ] && { echo "Error: signing failed (rekor 503)" >&2; exit 1; }
echo "tlog entry created"
SH
cat > "$D/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_LOG"
[ "$1" = api ] || exit 2; shift
if [ "$1" = "-X" ]; then exit 0; fi
jqexpr=""; while [ $# -gt 0 ]; do case "$1" in --jq) jqexpr="$2"; shift 2;; *) shift;; esac; done
jq -r "$jqexpr" "$STUB_VERSIONS"
SH
chmod +x "$D/bin/"*

# versions: 42 = this run's push of 1.0.1; 7 = an older 1.0.0; 9 = a
# 1.0.1 pushed before the run (an identical re-push answers with it).
cat > "$D/versions-fresh.json" <<'J'
[{"id":42,"created_at":"2026-10-14T10:05:00Z","metadata":{"container":{"tags":["1.0.1"]}}},
 {"id":7,"created_at":"2026-09-01T00:00:00Z","metadata":{"container":{"tags":["1.0.0"]}}}]
J
cat > "$D/versions-old.json" <<'J'
[{"id":9,"created_at":"2026-10-13T00:00:00Z","metadata":{"container":{"tags":["1.0.1"]}}}]
J
echo chart > "$D/tinycdi-1.0.1.tgz"

run() { # run <versions-file> [ENV=VAL...] -> rc, out (stdout); overrides last
  local v="$1"; shift
  : > "$LOG"
  out="$(env PATH="$D/bin:$PATH" STUB_LOG="$LOG" STUB_DIGEST="$DG" STUB_VERSIONS="$D/$v" \
    HELM="$D/bin/helm" CHART_NAME=tinycdi CHART_VERSION=1.0.1 \
    CHART_OCI=oci://ghcr.io/test/charts RUN_STARTED_AT=2026-10-14T10:00:00Z "$@" \
    bash "$SCRIPT" "$D/tinycdi-1.0.1.tgz" 2>"$D/err")"; rc=$?
}
deleted() { grep -E '^gh api -X DELETE ' "$LOG" | sed -E 's#.*/versions/##'; }

# 1. happy path: push, sign by digest, print only the ref, delete nothing.
run versions-fresh.json
[ "$rc" -eq 0 ] || { echo "FAIL: happy path exited $rc: $(cat "$D/err")"; fails=1; }
[ "$out" = "ref=ghcr.io/test/charts/tinycdi@$DG" ] \
  || { echo "FAIL: stdout is not exactly the ref line: '$out'"; fails=1; }
grep -q "^cosign sign --yes ghcr.io/test/charts/tinycdi@$DG\$" "$LOG" \
  || { echo "FAIL: chart not signed by digest"; fails=1; }
[ -z "$(deleted)" ] || { echo "FAIL: happy path deleted versions: $(deleted)"; fails=1; }

# 2. signing fails: this run's 1.0.1 (id 42) is deleted, nothing else.
run versions-fresh.json STUB_COSIGN_FAIL=1
[ "$rc" -ne 0 ] || { echo "FAIL: cosign failure did not fail the step"; fails=1; }
[ "$(deleted)" = "42" ] || { echo "FAIL: unsigned chart not rolled back (deleted='$(deleted)')"; fails=1; }
grep -q "packages/container/charts%2Ftinycdi/versions" "$LOG" \
  || { echo "FAIL: version lookup not on package charts%2Ftinycdi"; fails=1; }

# 3. digest lookup fails after the push: same rollback.
run versions-fresh.json STUB_NO_DIGEST=1
{ [ "$rc" -ne 0 ] && [ "$(deleted)" = "42" ]; } \
  || { echo "FAIL: unresolved digest did not roll back (rc=$rc deleted='$(deleted)')"; fails=1; }

# 4. helm push itself fails: whatever it may have tagged this run goes.
run versions-fresh.json STUB_HELM_FAIL=1
{ [ "$rc" -ne 0 ] && [ "$(deleted)" = "42" ]; } \
  || { echo "FAIL: failed push did not roll back (rc=$rc deleted='$(deleted)')"; fails=1; }

# 5. a 1.0.1 version that predates the run is never deleted.
run versions-old.json STUB_COSIGN_FAIL=1
{ [ "$rc" -ne 0 ] && [ -z "$(deleted)" ]; } \
  || { echo "FAIL: pre-run version touched (rc=$rc deleted='$(deleted)')"; fails=1; }

# 6. a malformed CHART_OCI is refused before anything is pushed.
run versions-fresh.json CHART_OCI=oci://docker.io/x
{ [ "$rc" -ne 0 ] && ! grep -q '^helm push' "$LOG"; } \
  || { echo "FAIL: foreign CHART_OCI accepted"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "push-sign-chart: all cases pass"; fi
exit "$fails"
