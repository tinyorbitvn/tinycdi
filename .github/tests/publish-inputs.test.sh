#!/usr/bin/env bash
# publish-inputs.test.sh — regression test for SUPF-1 (publish-job
# artifact input validation), ported from the artifact-overwrite PoC:
# artifacts merged with merge-multiple:true let a forged artifact
# overwrite a same-named release input. The pipeline now downloads each
# artifact by EXACT name into its own folder and collect-publish-inputs.sh
# requires exactly the expected file set per folder, strict
# repo@sha256 refs, and chart digests == image refs.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COLLECT="$ROOT/.github/scripts/collect-publish-inputs.sh"
STAMP="$ROOT/.github/scripts/stamp-image-digests.sh"
fails=0

VERSION="v0.1.0"
CHART_VERSION="0.1.0"
IMGS=(backend browser frontend kasm-adapter linux-base linux-desktop operator)
RELEASE_IMAGES='["backend","browser","frontend","kasm-adapter","linux-base","linux-desktop","operator"]'
KV="1.5.0"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

digest_for() { echo "sha256:$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"; }

# build_store <dir> — populate a clean artifact-download layout.
build_store() {
  local dl="$1"
  local img
  for img in "${IMGS[@]}"; do
    mkdir -p "$dl/image-ref-$img" "$dl/sbom-$img"
    echo "ghcr.io/tinyorbitvn/tinycdi-$img@$(digest_for "$img")" \
      > "$dl/image-ref-$img/$img.ref"
    echo "{\"spdxVersion\":\"SPDX-2.3\",\"name\":\"$img\"}" \
      > "$dl/sbom-$img/sbom-$img.spdx.json"
  done
  # dedicated SBOMs for the packaged chart and the release binaries (SEC-17)
  for extra in chart binaries; do
    mkdir -p "$dl/sbom-$extra"
    echo "{\"spdxVersion\":\"SPDX-2.3\",\"name\":\"$extra\"}" \
      > "$dl/sbom-$extra/sbom-$extra.spdx.json"
  done

  # packaged chart: real stamp script + real tar layout
  local cd_="$WORK/chart-src"
  mkdir -p "$cd_/tinycdi"
  {
    echo "images:"
    for img in backend browser frontend operator; do
      printf '  %s:\n    repository: tinyorbitvn/tinycdi-%s\n    digest: ""\n' "$img" "$img"
    done
    printf '  linuxBase:\n    repository: tinyorbitvn/tinycdi-linux-base\n    digest: ""\n'
    printf '  linuxDesktop:\n    repository: tinyorbitvn/tinycdi-linux-desktop\n    digest: ""\n'
    printf 'kasmAdapter:\n  image:\n    repository: tinyorbitvn/tinycdi-kasm-adapter\n    digest: ""\n'
  } > "$cd_/tinycdi/values.yaml"
  local src_refs="$WORK/src-refs"
  mkdir -p "$src_refs"
  for img in "${IMGS[@]}"; do
    cp "$dl/image-ref-$img/$img.ref" "$src_refs/$img.ref"
  done
  "$STAMP" "$cd_/tinycdi/values.yaml" "$src_refs" >/dev/null
  mkdir -p "$dl/release-chart"
  tar -czf "$dl/release-chart/tinycdi-$CHART_VERSION.tgz" -C "$cd_" tinycdi

  mkdir -p "$dl/release-assets"
  for f in "tinycdi-backend-$VERSION-linux-amd64" \
           "tinycdi-operator-$VERSION-linux-amd64" \
           "tinycdi-crds-$VERSION.yaml"; do
    echo "blob $f" > "$dl/release-assets/$f"
  done
  echo "kasmvnc source" > "$dl/release-assets/kasmvnc-$KV-corresponding-source.tar.gz"
  echo "deadbeef  kasmvnc-$KV-corresponding-source.tar.gz" \
    > "$dl/release-assets/kasmvnc-$KV-corresponding-source.tar.gz.sha256"
}

run_collect() { # run_collect <dl-dir> -> rc in $rc, output in $out
  rm -rf "$WORK/out"
  out="$(cd "$ROOT" && \
    RELEASE_IMAGES="$RELEASE_IMAGES" VERSION="$VERSION" \
    CHART_VERSION="$CHART_VERSION" KASMVNC_SRC=true \
    bash "$COLLECT" "$1" "$WORK/out" 2>&1)"
  rc=$?
}

expect_ok() {
  build_store "$WORK/dl"
  run_collect "$WORK/dl"
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: happy path rejected:"; echo "$out"; fails=1
    return
  fi
  [ -f "$WORK/out/bundle/tinycdi-$CHART_VERSION.tgz" ] \
    || { echo "FAIL: bundle lacks chart tgz"; fails=1; }
  [ -f "$WORK/out/bundle/kasmvnc-$KV-corresponding-source.tar.gz" ] \
    || { echo "FAIL: bundle lacks kasmvnc source tarball"; fails=1; }
  [ -f "$WORK/out/bundle/kasmvnc-$KV-corresponding-source.tar.gz.sha256" ] \
    || { echo "FAIL: bundle lacks kasmvnc sha256"; fails=1; }
  [ "$(find "$WORK/out/refs" -name '*.ref' | wc -l)" -eq "${#IMGS[@]}" ] \
    || { echo "FAIL: refs dir wrong"; fails=1; }
  [ "$(find "$WORK/out/sboms" -name '*.json' | wc -l)" -eq "$(( ${#IMGS[@]} + 2 ))" ] \
    || { echo "FAIL: sboms dir wrong"; fails=1; }
  for extra in chart binaries; do
    [ -f "$WORK/out/bundle/sbom-$extra.spdx.json" ] \
      || { echo "FAIL: bundle lacks sbom-$extra.spdx.json"; fails=1; }
  done
  echo "ok: happy path"
}

expect_fail() { # expect_fail <name> <stderr-substr> <mutator-fn>
  local name="$1" want="$2" mut="$3"
  rm -rf "$WORK/dl"; build_store "$WORK/dl"
  "$mut" "$WORK/dl"
  run_collect "$WORK/dl"
  if [ "$rc" -eq 0 ]; then
    echo "FAIL ($name): collect accepted poisoned inputs"
    fails=1
  elif [ -n "$want" ] && ! grep -qF "$want" <<< "$out"; then
    echo "FAIL ($name): expected error containing '$want', got:"; echo "$out"
    fails=1
  else
    echo "ok: $name"
  fi
}

mut_extra_artifact() { mkdir -p "$1/image-ref-zz"; echo x > "$1/image-ref-zz/zz.ref"; }
mut_extra_file()     { echo evil > "$1/image-ref-backend/evil.sh"; }
mut_bad_ref()        { echo "ghcr.io/evil/tinycdi-backend@$(digest_for backend)" > "$1/image-ref-backend/backend.ref"; }
mut_wrong_digest()   { echo "ghcr.io/tinyorbitvn/tinycdi-backend@sha256:$(printf 'f%.0s' $(seq 64))" \
                         > "$1/image-ref-backend/backend.ref"; }
mut_bad_chart() {
  # chart stamped with a different backend digest than the ref ships
  local cd_="$WORK/chart-evil"
  mkdir -p "$cd_/tinycdi"
  cp -r "$WORK/chart-src/tinycdi/." "$cd_/tinycdi/" 2>/dev/null || true
  mkdir -p "$cd_/tinycdi"
  {
    echo "images:"
    for img in backend browser frontend operator linuxBase linuxDesktop; do
      printf '  %s:\n    digest: "sha256:%s"\n' "$img" "$(printf 'e%.0s' $(seq 64))"
    done
    printf 'kasmAdapter:\n  image:\n    digest: "sha256:%s"\n' "$(printf 'e%.0s' $(seq 64))"
  } > "$cd_/tinycdi/values.yaml"
  rm -f "$1/release-chart/tinycdi-$CHART_VERSION.tgz"
  tar -czf "$1/release-chart/tinycdi-$CHART_VERSION.tgz" -C "$cd_" tinycdi
}
mut_no_kasmvnc()     { rm -f "$1/release-assets/kasmvnc-$KV-corresponding-source.tar.gz.sha256"; }
mut_bad_sbom()       { echo 'not json' > "$1/sbom-backend/sbom-backend.spdx.json"; }
mut_no_chart_sbom()  { rm -rf "$1/sbom-chart"; }
mut_bad_bins_sbom()  { echo 'not json' > "$1/sbom-binaries/sbom-binaries.spdx.json"; }
mut_missing_bin()    { rm -f "$1/release-assets/tinycdi-backend-$VERSION-linux-amd64"; }
# v0.2 removed the api/gateway commands — a stale binary must not ride along.
mut_stale_bin()      { local c=api; echo old > "$1/release-assets/tinycdi-$c-$VERSION-linux-amd64"; }
# a removed image (api/gateway/portal) must not slip into the publish set.
mut_removed_image()  { local i=portal; mkdir -p "$1/image-ref-$i"; echo "ghcr.io/tinyorbitvn/tinycdi-$i@$(digest_for "$i")" > "$1/image-ref-$i/$i.ref"; }
mut_stowaway_bundle(){ echo evil > "$1/release-chart/extra.tgz"; }

expect_ok
expect_fail "forged extra artifact folder" "artifact folder set" mut_extra_artifact
expect_fail "extra file inside artifact" "image-ref-backend" mut_extra_file
expect_fail "ref with foreign repo" "sha256" mut_bad_ref
expect_fail "ref digest != chart digest" "digest" mut_wrong_digest
expect_fail "chart stamped with wrong digests" "digest" mut_bad_chart
expect_fail "missing kasmvnc checksum" "KasmVNC" mut_no_kasmvnc
expect_fail "invalid sbom json" "not valid JSON" mut_bad_sbom
expect_fail "missing chart sbom artifact" "artifact folder set" mut_no_chart_sbom
expect_fail "invalid binaries sbom json" "not valid JSON" mut_bad_bins_sbom
expect_fail "missing release binary" "missing" mut_missing_bin
expect_fail "stale removed-component binary" "unexpected release-assets file" mut_stale_bin
expect_fail "removed image artifact" "artifact folder set" mut_removed_image
expect_fail "stowaway in chart artifact" "release-chart" mut_stowaway_bundle

# KASMVNC_SRC=false must reject the bundle and accept a store without it.
rm -rf "$WORK/dl"; build_store "$WORK/dl"
rm -f "$WORK/dl/release-assets"/kasmvnc-*
out="$(cd "$ROOT" && RELEASE_IMAGES="$RELEASE_IMAGES" VERSION="$VERSION" \
  CHART_VERSION="$CHART_VERSION" KASMVNC_SRC=false \
  bash "$COLLECT" "$WORK/dl" "$WORK/out" 2>&1)" && rc=0 || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL: no-kasmvnc store rejected"; echo "$out"; fails=1; }
rm -rf "$WORK/out"
mkdir -p "$WORK/dl2"; build_store "$WORK/dl2"
out="$(cd "$ROOT" && RELEASE_IMAGES="$RELEASE_IMAGES" VERSION="$VERSION" \
  CHART_VERSION="$CHART_VERSION" KASMVNC_SRC=false \
  bash "$COLLECT" "$WORK/dl2" "$WORK/out" 2>&1)" && rc=0 || rc=$?
if [ "$rc" -eq 0 ]; then
  echo "FAIL: kasmvnc bundle accepted when no GPL image is released"; fails=1
else
  echo "ok: kasmvnc bundle rejected without GPL images"
fi

if [ "$fails" -eq 0 ]; then echo "publish-inputs: all guards pass"; fi
exit "$fails"
