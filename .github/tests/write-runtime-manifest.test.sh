#!/usr/bin/env bash
# write-runtime-manifest.test.sh — the runtime-images.json contract the
# release train attaches to its GitHub Release (D27/P7): the file parses
# as JSON, carries the three runtime images (linux-base, linux-desktop,
# browser) with repo ref, digest and rt tag, and stamps the browser
# versions the images actually installed — read off each image's SPDX
# SBOM (browser: chromium + firefox; linux-desktop: firefox), never the
# Dockerfile pins that asked for them.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GEN="$ROOT/.github/scripts/write-runtime-manifest.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0
EARLIEST="$ROOT/.github/scripts/earliest-built-at.sh"

# real-shaped 64-hex digests
DGD="$(printf 'a%.0s' $(seq 64))"
DGB="$(printf 'b%.0s' $(seq 64))"
DGL="$(printf 'c%.0s' $(seq 64))"

mkdir -p "$D/refs" "$D/sboms"
echo "ghcr.io/tinyorbitvn/tinycdi-linux-base@sha256:$DGL"    > "$D/refs/linux-base.ref"
echo "ghcr.io/tinyorbitvn/tinycdi-linux-desktop@sha256:$DGD" > "$D/refs/linux-desktop.ref"
echo "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$DGB"       > "$D/refs/browser.ref"

# SPDX-2.3 fixtures in syft's shape (name + versionInfo per package).
# The lookup keys on the EXACT package name — chromium-sandbox and
# chromium-common must not satisfy the chromium query.
cat > "$D/sboms/sbom-linux-base.spdx.json" <<'EOF'
{"spdxVersion":"SPDX-2.3","name":"linux-base","packages":[
 {"SPDXID":"SPDXRef-1","name":"kasmvncserver","versionInfo":"1.5.0"},
 {"SPDXID":"SPDXRef-2","name":"xvfb","versionInfo":"2:21.1.7-3+deb12u9"}]}
EOF
cat > "$D/sboms/sbom-linux-desktop.spdx.json" <<'EOF'
{"spdxVersion":"SPDX-2.3","name":"linux-desktop","packages":[
 {"SPDXID":"SPDXRef-1","name":"xfce4-session","versionInfo":"4.18.3-1"},
 {"SPDXID":"SPDXRef-2","name":"firefox-esr","versionInfo":"153.4.0esr-1~deb12u1"}]}
EOF
cat > "$D/sboms/sbom-browser.spdx.json" <<'EOF'
{"spdxVersion":"SPDX-2.3","name":"browser","packages":[
 {"SPDXID":"SPDXRef-1","name":"openbox","versionInfo":"3.6.1-9+deb12u1"},
 {"SPDXID":"SPDXRef-2","name":"chromium","versionInfo":"154.0.8037.92-1~deb12u1"},
 {"SPDXID":"SPDXRef-3","name":"chromium-sandbox","versionInfo":"154.0.8037.92-1~deb12u1"},
 {"SPDXID":"SPDXRef-4","name":"chromium-common","versionInfo":"154.0.8037.92-1~deb12u1"},
 {"SPDXID":"SPDXRef-5","name":"firefox-esr","versionInfo":"153.4.0esr-1~deb12u1"}]}
EOF

rc=0; out=""
run_gen() {
  out="$(RT_TAG=rt-20261020.1 BUILT_AT=2026-10-20T03:10:00Z \
    bash "$GEN" "$@" 2>&1)"; rc=$?
}

# --- happy path ---
run_gen "$D/refs" "$D/sboms" "$D/runtime-images.json"
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
         | .chromium == "154.0.8037.92-1~deb12u1" and .firefox == "153.4.0esr-1~deb12u1")
    and (.images[] | select(.name == "linux-desktop")
         | .firefox == "153.4.0esr-1~deb12u1" and (has("chromium") | not))
    and (.images[] | select(.name == "linux-base") | (has("firefox") or has("chromium")) | not)
  ' "$D/runtime-images.json" >/dev/null \
    || { echo "FAIL: manifest shape mismatch:"; cat "$D/runtime-images.json"; fails=1; }
fi

# --- malformed ref rejected, no output file ---
mkdir -p "$D/refs-badref"; cp "$D/refs/"*.ref "$D/refs-badref/"
echo "ghcr.io/evil/tinycdi-browser@sha256:$DGB" > "$D/refs-badref/browser.ref"
run_gen "$D/refs-badref" "$D/sboms" "$D/bad.json"
if [ "$rc" -eq 0 ] || [ -e "$D/bad.json" ]; then
  echo "FAIL: foreign/malformed ref accepted"; fails=1
fi

# --- missing RT_TAG rejected ---
out="$(bash "$GEN" "$D/refs" "$D/sboms" "$D/notag.json" 2>&1)" && rc=0 || rc=$?
[ "$rc" -ne 0 ] || { echo "FAIL: missing RT_TAG accepted"; fails=1; }

# --- the desktop image without a firefox-esr package is an error ---
mkdir -p "$D/sboms-nofx"; cp "$D/sboms/"*.json "$D/sboms-nofx/"
jq 'del(.packages[] | select(.name == "firefox-esr"))' \
  "$D/sboms/sbom-linux-desktop.spdx.json" > "$D/sboms-nofx/sbom-linux-desktop.spdx.json"
run_gen "$D/refs" "$D/sboms-nofx" "$D/nofx.json"
{ [ "$rc" -ne 0 ] && [ ! -e "$D/nofx.json" ]; } \
  || { echo "FAIL: linux-desktop entry without firefox-esr in the SBOM accepted"; fails=1; }

# --- a missing SBOM file is an error ---
mkdir -p "$D/sboms-partial"
cp "$D/sboms/sbom-linux-base.spdx.json" "$D/sboms/sbom-linux-desktop.spdx.json" \
  "$D/sboms-partial/"
run_gen "$D/refs" "$D/sboms-partial" "$D/nosbom.json"
{ [ "$rc" -ne 0 ] && [ ! -e "$D/nosbom.json" ]; } \
  || { echo "FAIL: manifest written despite a missing browser SBOM"; fails=1; }

# --- an ambiguous SBOM (two distinct versions of one package) is an error ---
mkdir -p "$D/sboms-dup"; cp "$D/sboms/"*.json "$D/sboms-dup/"
jq '.packages += [{"SPDXID":"SPDXRef-9","name":"firefox-esr","versionInfo":"152.9.0esr-1~deb12u1"}]' \
  "$D/sboms/sbom-browser.spdx.json" > "$D/sboms-dup/sbom-browser.spdx.json"
run_gen "$D/refs" "$D/sboms-dup" "$D/dup.json"
{ [ "$rc" -ne 0 ] && [ ! -e "$D/dup.json" ]; } \
  || { echo "FAIL: SBOM with two distinct firefox-esr versions accepted"; fails=1; }

# --- a non-JSON SBOM is an error ---
mkdir -p "$D/sboms-badjson"; cp "$D/sboms/"*.json "$D/sboms-badjson/"
echo 'not json' > "$D/sboms-badjson/sbom-browser.spdx.json"
run_gen "$D/refs" "$D/sboms-badjson" "$D/badjson.json"
{ [ "$rc" -ne 0 ] && [ ! -e "$D/badjson.json" ]; } \
  || { echo "FAIL: non-JSON SBOM accepted"; fails=1; }

# --- R1-09: BUILT_AT is required — the publish clock is never builtAt ---
out="$(RT_TAG=rt-20261020.1 bash "$GEN" "$D/refs" "$D/sboms" "$D/nobuilt.json" 2>&1)" && rc=0 || rc=$?
{ [ "$rc" -ne 0 ] && [ ! -e "$D/nobuilt.json" ]; } \
  || { echo "FAIL: unset BUILT_AT accepted (builtAt would be the publish time)"; fails=1; }

# --- R1-09: BUILT_AT must be a UTC RFC 3339 second ---
out="$(RT_TAG=rt-20261020.1 BUILT_AT=yesterday \
  bash "$GEN" "$D/refs" "$D/sboms" "$D/badbuilt.json" 2>&1)" && rc=0 || rc=$?
{ [ "$rc" -ne 0 ] && [ ! -e "$D/badbuilt.json" ]; } \
  || { echo "FAIL: malformed BUILT_AT accepted"; fails=1; }

# --- R1-09: builtAt is the EARLIEST build-job time of the train ---
mkdir -p "$D/refs-ba"; cp "$D/refs/"*.ref "$D/refs-ba/"
printf '2026-09-01T04:23:00Z\n' > "$D/refs-ba/linux-base.built-at"
printf '2026-09-01T04:41:10Z\n' > "$D/refs-ba/linux-desktop.built-at"
printf '2026-09-01T04:40:02Z\n' > "$D/refs-ba/browser.built-at"
ba="$(bash "$EARLIEST" "$D/refs-ba" linux-base linux-desktop browser 2>&1)" && rc=0 || rc=$?
if [ "$rc" -ne 0 ] || [ "$ba" != "2026-09-01T04:23:00Z" ]; then
  echo "FAIL: earliest-built-at.sh gave '$ba' (rc=$rc), want 2026-09-01T04:23:00Z"; fails=1
else
  out="$(RT_TAG=rt-20261020.1 BUILT_AT="$ba" \
    bash "$GEN" "$D/refs-ba" "$D/sboms" "$D/early.json" 2>&1)" && rc=0 || rc=$?
  { [ "$rc" -eq 0 ] && jq -e '.builtAt == "2026-09-01T04:23:00Z"' "$D/early.json" >/dev/null 2>&1; } \
    || { echo "FAIL: manifest builtAt is not the build-job time: $out"; fails=1; }
fi

# --- R1-09: a missing or malformed build time fails closed ---
mkdir -p "$D/refs-noba"; cp "$D/refs-ba/"* "$D/refs-noba/"; rm "$D/refs-noba/browser.built-at"
bash "$EARLIEST" "$D/refs-noba" linux-base linux-desktop browser >/dev/null 2>&1 \
  && { echo "FAIL: earliest-built-at.sh accepted a missing browser.built-at"; fails=1; }
mkdir -p "$D/refs-badba"; cp "$D/refs-ba/"* "$D/refs-badba/"
printf '2026-09-31T25:00:00Z\n' > "$D/refs-badba/browser.built-at"
bash "$EARLIEST" "$D/refs-badba" linux-base linux-desktop browser >/dev/null 2>&1 \
  && { echo "FAIL: earliest-built-at.sh accepted an impossible date"; fails=1; }
printf '2026-09-01 04:40:02\n' > "$D/refs-badba/browser.built-at"
bash "$EARLIEST" "$D/refs-badba" linux-base linux-desktop browser >/dev/null 2>&1 \
  && { echo "FAIL: earliest-built-at.sh accepted a non-RFC 3339 time"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "write-runtime-manifest: all cases pass"; fi
exit "$fails"
