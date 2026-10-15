#!/usr/bin/env bash
# next-rt-tag.test.sh — the rt-YYYYMMDD.N numbering contract (backlog
# 15): N counts successful publishes only, i.e. the highest existing
# rt-<date>.* tag on the package + 1. Other dates, non-rt tags and
# lookalikes never contribute; an empty/absent tag list starts the day
# at .1; malformed input and a failed registry lookup fail closed.
# The real registry path (token + paginated tags/list) is driven too,
# through a stub curl on PATH: a page without a Link rel="next" header
# ends pagination instead of killing the script, pages merge before
# max+1, and transport failures still fail closed.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GEN="$ROOT/.github/scripts/next-rt-tag.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0
rc=0; out=""

run_gen() { out="$(bash "$GEN" "$@" 2>&1)"; rc=$?; }

expect() { # expect <desc> <want-tag> <tags-json>
  local desc="$1" want="$2" json="$3"
  printf '%s' "$json" > "$D/in.json"
  run_gen --date 20261020 --tags-json "$D/in.json"
  if [ "$rc" -ne 0 ] || [ "$out" != "$want" ]; then
    echo "FAIL: $desc (rc=$rc, out='$out', want '$want')"; fails=1
  fi
}

expect "empty package starts at .1"          rt-20261020.1  '{"tags":[]}'
expect "tags:null starts at .1"              rt-20261020.1  '{"tags":null}'
expect "missing tags key starts at .1"       rt-20261020.1  '{"name":"x"}'
expect "consecutive existing tags"           rt-20261020.4  '["rt-20261020.1","rt-20261020.2","rt-20261020.3"]'
expect "max+1 over a gapped series"          rt-20261020.6  '{"tags":["rt-20261020.1","rt-20261020.5"]}'
expect "numeric not lexical (.10 beats .9)"  rt-20261020.11 '{"tags":["rt-20261020.9","rt-20261020.10"]}'
expect "other dates/prefixes ignored"        rt-20261020.2  '{"tags":["rt-20261019.7","rt-20261021.4","sha-abc","main","rt-20261020.1"]}'
expect "rt lookalikes ignored"               rt-20261020.2  '{"tags":["rt-2026102.9","rt-202610200.9","rt-20261020.x","rt-20261020.","foo","rt-20261020.1"]}'

# --- stdin source ---
out="$(printf '%s' '{"tags":["rt-20261020.2"]}' \
  | bash "$GEN" --date 20261020 --tags-json - 2>&1)"; rc=$?
{ [ "$rc" -eq 0 ] && [ "$out" = "rt-20261020.3" ]; } \
  || { echo "FAIL: --tags-json - (stdin) (rc=$rc, out='$out')"; fails=1; }

# --- failures must fail closed ---
printf '%s' '{"tags":[]}' > "$D/in.json"
run_gen --date 2026-10-20 --tags-json "$D/in.json"
[ "$rc" -ne 0 ] || { echo "FAIL: non-YYYYMMDD date accepted"; fails=1; }
run_gen --tags-json "$D/in.json"
[ "$rc" -ne 0 ] || { echo "FAIL: missing --date accepted"; fails=1; }
run_gen --date 20261020 --tags-json "$D/nope.json"
[ "$rc" -ne 0 ] || { echo "FAIL: missing tags file accepted"; fails=1; }
printf '%s' 'not json' > "$D/bad.json"
run_gen --date 20261020 --tags-json "$D/bad.json"
[ "$rc" -ne 0 ] || { echo "FAIL: malformed tags JSON accepted"; fails=1; }
run_gen --date 20261020 --bogus
[ "$rc" -ne 0 ] || { echo "FAIL: unknown argument accepted"; fails=1; }

# --- registry pagination path (stub curl on PATH, no network) ---
# The shim answers /token with a fixed grant and serves tags/list pages
# from $STUB_DIR: page<N>.json bodies, optional page<N>.link values for
# the Link response header (absent = last page). STUB_FAIL kills every
# call; STUB_FAIL_AT=N kills tags/list from page N on (exit 22 like
# `curl -f` on an HTTP error).
mkdir -p "$D/shim"
cat > "$D/shim/curl" <<'EOF'
#!/usr/bin/env bash
hdrs="" url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -D)    hdrs="$2"; shift 2 ;;
    -H|-u) shift 2 ;;
    http*) url="$1"; shift ;;
    *)     shift ;;
  esac
done
[ -z "${STUB_FAIL:-}" ] || exit 22
case "$url" in
  *"/token"*) printf '%s' '{"token":"stub"}' ;;
  *"tags/list"*)
    n=0; [ ! -f "$STUB_DIR/seq" ] || n="$(cat "$STUB_DIR/seq")"
    n=$((n + 1)); echo "$n" > "$STUB_DIR/seq"
    if [ -n "${STUB_FAIL_AT:-}" ] && [ "$n" -ge "$STUB_FAIL_AT" ]; then exit 22; fi
    printf 'HTTP/1.1 200 OK\r\n' > "$hdrs"
    if [ -f "$STUB_DIR/page$n.link" ]; then
      printf 'Link: %s\r\n' "$(cat "$STUB_DIR/page$n.link")" >> "$hdrs"
    fi
    cat "$STUB_DIR/page$n.json" ;;
  *) exit 22 ;;
esac
EOF
chmod +x "$D/shim/curl"

mkpage() { # mkpage <dir> <n> <json> [link-value]
  mkdir -p "$1"
  printf '%s' "$3" > "$1/page$2.json"
  if [ $# -ge 4 ]; then printf '%s' "$4" > "$1/page$2.link"; fi
}

run_reg() { # run_reg <stubdir>: drive the real registry path via the shim
  out="$(env PATH="$D/shim:$PATH" RT_TAG_REGISTRY=stub.invalid GH_TOKEN="" \
    STUB_DIR="$1" bash "$GEN" --date 20261020 2>&1)"; rc=$?
}

# last/only page carries no Link rel="next" header (the run-37191523760
# failure: grep's no-match exit 1 killed the loop under pipefail)
mkpage "$D/one" 1 '{"tags":["rt-20261020.2","v1.2.3","main"]}'
run_reg "$D/one"
{ [ "$rc" -eq 0 ] && [ "$out" = "rt-20261020.3" ]; } \
  || { echo "FAIL: single page without Link header (rc=$rc, out='$out')"; fails=1; }

# rel="next" page, then a last page without one: bodies merge before max+1
mkpage "$D/two" 1 '{"tags":["rt-20261020.1","rt-20261020.4"]}' \
  '</v2/x/tags/list?last=rt-20261020.4&n=1000>; rel="next"'
mkpage "$D/two" 2 '{"tags":["rt-20261020.7"]}'
run_reg "$D/two"
{ [ "$rc" -eq 0 ] && [ "$out" = "rt-20261020.8" ]; } \
  || { echo "FAIL: two-page tags/list merge (rc=$rc, out='$out')"; fails=1; }

# transport failures fail closed: token call, first page, mid-pagination
mkpage "$D/dead" 1 '{"tags":["rt-20261020.1"]}'
STUB_FAIL=1 run_reg "$D/dead"
[ "$rc" -ne 0 ] || { echo "FAIL: token curl failure accepted"; fails=1; }
STUB_FAIL_AT=1 run_reg "$D/dead"
[ "$rc" -ne 0 ] || { echo "FAIL: tags/list curl failure accepted"; fails=1; }
mkpage "$D/mid" 1 '{"tags":["rt-20261020.1"]}' \
  '</v2/x/tags/list?last=rt-20261020.1&n=1000>; rel="next"'
STUB_FAIL_AT=2 run_reg "$D/mid"
[ "$rc" -ne 0 ] || { echo "FAIL: mid-pagination curl failure accepted"; fails=1; }

# --- registry lookup fails closed (dead port, no guessing) ---
out="$(RT_TAG_REGISTRY=127.0.0.1:1 GH_TOKEN="" \
  bash "$GEN" --date 20261020 2>&1)"; rc=$?
[ "$rc" -ne 0 ] || { echo "FAIL: unreachable registry did not fail closed"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "next-rt-tag: all cases pass"; fi
exit "$fails"
