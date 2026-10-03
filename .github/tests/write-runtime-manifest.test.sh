#!/usr/bin/env bash
# write-runtime-manifest.test.sh — the runtime-images.json contract the
# release train attaches to its GitHub Release (D27/P7): the file parses
# as JSON, carries the three runtime images (linux-base, linux-desktop,
# browser) with repo ref, digest and rt tag, and stamps the engine
# versions (browser: chromium + firefox; linux-desktop: firefox).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GEN="$ROOT/.github/scripts/write-runtime-manifest.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

# real-shaped 64-hex digests
DGD="$(printf 'a%.0s' $(seq 64))"
DGB="$(printf 'b%.0s' $(seq 64))"
DGL="$(printf 'c%.0s' $(seq 64))"

mkdir -p "$D/refs"
echo "ghcr.io/tinyorbitvn/tinycdi-linux-base@sha256:$DGL"    > "$D/refs/linux-base.ref"
echo "ghcr.io/tinyorbitvn/tinycdi-linux-desktop@sha256:$DGD" > "$D/refs/linux-desktop.ref"
echo "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$DGB"       > "$D/refs/browser.ref"
cat > "$D/Dockerfile" <<'EOF'
ARG CHROMIUM_APT_VERSION=154.0.8037.92-1~deb12u1
ARG FIREFOX_ESR_APT_VERSION=153.4.0esr-1~deb12u1
EOF
cat > "$D/Dockerfile.desktop" <<'EOF'
ARG FIREFOX_ESR_APT_VERSION=153.4.0esr-1~deb12u1
EOF

rc=0; out=""
run_gen() {
  out="$(RT_TAG=rt-20261020.1 BUILT_AT=2026-10-20T03:10:00Z \
    BROWSER_DOCKERFILE="$D/Dockerfile" DESKTOP_DOCKERFILE="$D/Dockerfile.desktop" \
    bash "$GEN" "$@" 2>&1)"; rc=$?
}

# --- happy path ---
run_gen "$D/refs" "$D/runtime-images.json"
if [ "$rc" -ne 0 ]; then
  echo "FAIL: manifest writer exited $rc: $out"; fails=1
elif ! jq -e . "$D/runtime-images.json" >/dev/null 2>&1; then
  echo "FAIL: output is not valid JSON"; fails=1
else
  jq -e --arg dgd "$DGD" --arg dgb "$DGB" --arg dgl "$DGL" '
    .builtAt == "2026-10-20T03:10:00Z"
    and (.images | length == 3)
    and ([.images[].name] == ["linux-base", "linux-desktop", "browser"])
    and (all(.images[]; .ref | test("^ghcr\\.io/tinyorbitvn/tinycdi-[a-z0-9-]+$")))
    and (all(.images[]; .digest | test("^sha256:[0-9a-f]{64}$")))
    and (all(.images[]; .tag == "rt-20261020.1"))
    and (.images[] | select(.name == "linux-desktop") | .digest == "sha256:" + $dgd)
    and (.images[] | select(.name == "browser") | .digest == "sha256:" + $dgb)
    and (.images[] | select(.name == "linux-base") | .digest == "sha256:" + $dgl)
    and (.images[] | select(.name == "browser")
         | .chromium == "154.0.8037.92" and .firefox == "153.4.0esr")
    and (.images[] | select(.name == "linux-desktop")
         | .firefox == "153.4.0esr" and (has("chromium") | not))
    and (.images[] | select(.name == "linux-base") | (has("firefox") or has("chromium")) | not)
  ' "$D/runtime-images.json" >/dev/null \
    || { echo "FAIL: manifest shape mismatch:"; cat "$D/runtime-images.json"; fails=1; }
fi

# --- malformed ref rejected, no output file ---
echo "ghcr.io/evil/tinycdi-browser@sha256:$DGB" > "$D/refs/browser.ref"
run_gen "$D/refs" "$D/bad.json"
if [ "$rc" -eq 0 ] || [ -e "$D/bad.json" ]; then
  echo "FAIL: foreign/malformed ref accepted"; fails=1
fi

# --- missing RT_TAG rejected ---
out="$(BROWSER_DOCKERFILE="$D/Dockerfile" DESKTOP_DOCKERFILE="$D/Dockerfile.desktop" \
  bash "$GEN" "$D/refs" "$D/notag.json" 2>&1)" && rc=0 || rc=$?
[ "$rc" -ne 0 ] || { echo "FAIL: missing RT_TAG accepted"; fails=1; }

# --- a desktop Dockerfile without the firefox pin is an error ---
echo "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$DGB" > "$D/refs/browser.ref"
: > "$D/Dockerfile.nopin"
out="$(RT_TAG=rt-20261020.1 BROWSER_DOCKERFILE="$D/Dockerfile" DESKTOP_DOCKERFILE="$D/Dockerfile.nopin" \
  bash "$GEN" "$D/refs" "$D/nopin.json" 2>&1)" && rc=0 || rc=$?
{ [ "$rc" -ne 0 ] && [ ! -e "$D/nopin.json" ]; } \
  || { echo "FAIL: linux-desktop manifest entry without a firefox pin accepted"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "write-runtime-manifest: all cases pass"; fi
exit "$fails"
