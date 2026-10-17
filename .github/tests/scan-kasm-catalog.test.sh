#!/usr/bin/env bash
# scan-kasm-catalog.test.sh — regression coverage for the engine-version
# read in .github/scripts/scan-kasm-catalog.sh (KASM-2), with docker and
# trivy stubbed: an unreadable version must fail the gate with a message,
# a transient empty read is retried once, and a readable version below the
# floor still fails.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCAN="$ROOT/.github/scripts/scan-kasm-catalog.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"
echo "kasmweb/chrome@$DIGEST chromium 154 browser" > "$D/cat"
printf '#!/bin/sh\nexit 0\n' > "$D/trivy"; chmod +x "$D/trivy"

# docker stub: `run` prints the lines of $D/versions one per call
# (an empty line prints nothing); pull/rmi succeed.
cat > "$D/docker" <<STUB
#!/bin/sh
case "\$1" in
  run) n=\$(cat "$D/calls" 2>/dev/null || echo 0); n=\$((n+1)); echo \$n > "$D/calls"
       sed -n "\${n}p" "$D/versions" | grep . || exit 1 ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$D/docker"

run() { # run <expect-rc> <expect-grep> <desc> <version lines...>
  local want="$1" pat="$2" desc="$3"; shift 3
  printf '%s\n' "$@" > "$D/versions"; rm -f "$D/calls"
  out="$(DOCKER="$D/docker" TRIVY="$D/trivy" ENGINE_RETRY_DELAY=0 bash "$SCAN" "$D/cat" 2>&1)"; rc=$?
  if [ "$rc" != "$want" ] || ! grep -q -- "$pat" <<<"$out"; then
    echo "FAIL: $desc (rc=$rc)"; echo "$out" | tail -5; fails=1
  fi
}

run 1 "could not determine chromium version" "unreadable version fails with a message" "" ""
run 1 "could not determine chromium version" "unparsable version fails with a message" "Chromium unknown" "Chromium unknown"
run 0 "major=155" "empty first read is retried" "" "Chromium 155.0.8059.79 built on Debian"
run 1 "below the freshness floor" "stale engine fails" "Chromium 150.0.7871.100"

[ "$fails" -eq 0 ] && echo "scan-kasm-catalog: version-read failures report and retry"
exit "$fails"
