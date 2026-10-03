#!/usr/bin/env bash
# release-topology.test.sh — structural guards on the supply-chain pipeline.
#
# Regression coverage for the workflow-level SEC fixes; coarse greps on
# purpose — each assertion names the rule it protects.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REL="$ROOT/.github/workflows/release.yml"
IMG="$ROOT/.github/workflows/images.yml"
CI="$ROOT/.github/workflows/ci.yml"
FRESH="$ROOT/.github/workflows/runtime-freshness.yml"
TRAIN="$ROOT/.github/workflows/runtime-images.yml"
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
order() { # order <desc> <file> <earlier-regex> <later-regex>
  local desc="$1" file="$2" a="$3" b="$4" la lb
  la="$(grep -nE "$a" "$file" | head -1 | cut -d: -f1)"
  lb="$(grep -nE "$b" "$file" | head -1 | cut -d: -f1)"
  if [ -z "$la" ] || [ -z "$lb" ] || [ "$la" -ge "$lb" ]; then
    echo "FAIL: $desc"; fails=1
  fi
}

# SEC-04: builds push by digest only; scanners run in a dedicated job that
# holds no packages:write / id-token / attestations / registry secrets.
chk "release: digest-only push" "$REL" 'push-by-digest=true'
chk_absent "release: no mutable tag on build push" "$REL" 'tags:.*IMAGE_TAGS'
chk "release: scan job is packages:read" "$REL" 'packages: read'
chk "release: publish gated by environment" "$REL" 'environment: release'
chk_absent "release: no trivy-action (unpinned runtime binary)" "$REL" 'uses:.*trivy-action'
# Job-level env entries sit at 6 columns ("    env:" + key); workflow-level
# at 2. Step-scoped secrets (10 columns) are the allowed pattern.
chk_absent "release: no job-level registry secrets" "$REL" '^ {2,6}(CRHUB|BASE_MIRROR)_[A-Z]+:'
chk "release: chart job needs image jobs" "$REL" 'needs: \[prepare, base, images\]'

# SEC-16: no shared GHA cache on release image builds.
chk_absent "release: no gha cache" "$REL" 'cache-(from|to):\s*>?-?\s*$?\s*\$?\{?\{?.*type=gha|cache-(from|to): type=gha'

# SEC-17: cosign sign/attest present; GitHub provenance gated on the var.
chk "release: cosign sign" "$REL" 'cosign sign'
chk "release: cosign attest (SBOM)" "$REL" 'cosign attest'
chk "release: provenance gated on ATTESTATIONS_ENABLED" "$REL" "ATTESTATIONS_ENABLED == 'true'"

# SEC-18: promote uses the stripped chart version (:X.Y.Z, no v) and :latest
# is conditional on non-prerelease; digests are stamped into the chart.
chk "release: tag uses chart_version (no v prefix)" "$REL" 'imagetools create.*CHART_VERSION|\$CHART_VERSION'
chk "release: latest only when not prerelease" "$REL" 'PRERELEASE'
chk "release: digest stamping script wired" "$REL" 'stamp-image-digests.sh'

# SEC-15 + SUPR-11: main-ancestry check resolves refs/heads/main to a SHA
# first — the bare name is ambiguous if a `main` tag ever exists.
chk "release: ancestry check" "$REL" 'compare/\$\{GITHUB_SHA\}\.\.\.\$\{MAIN_SHA\}|merge-base --is-ancestor'
chk "release: resolves refs/heads/main to a SHA" "$REL" 'git/ref/heads/main'

# SUPR-12: real publishes refuse an existing tag / GHCR version / release;
# SUPF-9: those existence checks fail CLOSED — only HTTP 404 means absent.
chk "release: refuses existing tag" "$REL" 'git/ref/tags/\$VERSION'
chk "release: refuses existing GHCR versions" "$REL" 'packages/container/.*/versions'
chk "release: refuses existing GH release" "$REL" 'releases/tags/\$VERSION'
chk "release: existence checks fail closed" "$REL" 'exists_or_die'
chk "release: 404 means absent" "$REL" 'HTTP 404'
chk_absent "release: no fail-open existence check" "$REL" 'gh api "repos/.*(tags|releases)/.*>/dev/null 2>&1'

# SUPF-2: dispatches are rehearsals only — the v* tag ruleset grants the
# Actions app no bypass, so dry_run=false must be refused up front.
chk "release: dispatch is dry-run only" "$REL" 'workflow_dispatch is dry-run only'

# SUPR-1: chart digest resolved from the registry, not parsed from helm
# push stdout (helm 4 prints Pushed:/Digest: to stderr).
chk "release: chart digest via imagetools inspect" "$REL" 'imagetools inspect'
chk_absent "release: no stdout digest parse" "$REL" 'out=.*helm.*push|helm push.*\$\('

# SUPR-2 + SUPF-1: publish downloads every input by EXACT artifact name
# (no pattern/merge-multiple — a forged artifact could overwrite a
# same-named file mid-merge; see .github/tests/publish-inputs.test.sh),
# validates the strict allowlist and cross-checks the chart's stamped
# digests against the image refs before pushing/signing/releasing.
chk_absent "release: no merge-multiple downloads" "$REL" 'merge-multiple:'
chk_absent "images: no merge-multiple downloads" "$IMG" 'merge-multiple:'
chk_absent "release: no artifact glob downloads" "$REL" 'pattern:'
chk_absent "images: no artifact glob downloads" "$IMG" 'pattern:'
chk "release: publish downloads by exact name" "$REL" 'gh run download'
chk "release: strict input validation wired" "$REL" 'collect-publish-inputs\.sh'
[ -x "$ROOT/.github/scripts/collect-publish-inputs.sh" ] \
  || { echo "FAIL: collect-publish-inputs.sh missing/not executable"; fails=1; }
[ -x "$ROOT/.github/scripts/validate-image-refs.sh" ] \
  || { echo "FAIL: validate-image-refs.sh missing/not executable"; fails=1; }
chk "release: ref regex enforced" "$ROOT/.github/scripts/validate-image-refs.sh" \
  'ghcr\\\.io/tinyorbitvn/tinycdi-.*@sha256:\[0-9a-f\]'
chk "release: chart digest cross-check" "$ROOT/.github/scripts/collect-publish-inputs.sh" 'digest'
chk "images: promote validates refs" "$IMG" 'validate-image-refs\.sh'
chk_absent "release: no scan outputs in publish bundle" "$REL" 'pattern: trivy-|merge.*sbom.*bundle'

# SUPR-13: digests are signed + attested BEFORE tags are promoted.
order "release: sign before tag promotion" "$REL" 'cosign sign images' 'promote image tags'
order "images: sign before tag promotion" "$IMG" 'cosign sign images' 'promote tags \('
order "release: chart push before tag promotion" "$REL" 'push helm chart to OCI' 'promote image tags'

# SUPR-9: one docker logout per registry, never || true / two args.
chk_absent "release: no masked logout" "$REL" 'docker logout .*\|\|'
chk_absent "release: no multi-registry logout" "$REL" 'docker logout [^ ]+ +[^ ]'
chk_absent "images: no masked logout" "$IMG" 'docker logout .*\|\|'
chk_absent "images: no multi-registry logout" "$IMG" 'docker logout [^ ]+ +[^ ]'

# SUPR-10: setup-buildx only in the build jobs (2 legs each) — never in
# publish/promote; the BuildKit image is digest-pinned.
n="$(grep -c 'setup-buildx-action' "$REL")"
[ "$n" -eq 2 ] || { echo "FAIL: release.yml setup-buildx count=$n (want 2)"; fails=1; }
n="$(grep -c 'setup-buildx-action' "$IMG")"
[ "$n" -eq 2 ] || { echo "FAIL: images.yml setup-buildx count=$n (want 2)"; fails=1; }
chk "release: buildkit pinned by digest" "$REL" 'moby/buildkit:buildx-stable-1@sha256:[0-9a-f]{64}'
chk "images: buildkit pinned by digest" "$IMG" 'moby/buildkit:buildx-stable-1@sha256:[0-9a-f]{64}'

# SUPR-3/SEC-44: scan-time expiry — the effective ignorefile is generated
# per run and passed via --ignorefile; SUPR-8: empty --config so a repo
# trivy.yaml/.syft.yaml can never steer the tools.
chk "release: scan generates effective ignorefile" "$REL" 'check-trivyignore\.sh.*--out|--out.*trivyignore'
chk "images: scan generates effective ignorefile" "$IMG" 'check-trivyignore\.sh.*--out|--out.*trivyignore'
chk "release: trivy --ignorefile" "$REL" '--ignorefile'
chk "images: trivy --ignorefile" "$IMG" '--ignorefile'
chk "release: trivy --config empty" "$REL" '--config .*trivy-empty\.yaml'
chk "images: trivy --config empty" "$IMG" '--config .*trivy-empty\.yaml'
chk "release: syft --config empty" "$REL" 'syft.*--config|--config .*syft-empty\.yaml'
chk "images: syft --config empty" "$IMG" 'syft.*--config|--config .*syft-empty\.yaml'
# SUPR-8: no tool config files may exist in the repo at all.
for f in trivy.yaml .trivy.yaml trivy.yml .trivy.yml syft.yaml .syft.yaml syft.yml .syft.yml .trivyignore.yaml; do
  [ -e "$ROOT/$f" ] && { echo "FAIL: $f exists — scanners must not read repo config"; fails=1; }
done

# SUPR-4: the released image set is a tracked file; the browser ships in
# v0.1.0 (chromium .92) and, with linux-desktop (V3.26), FROMs linux-base,
# so all three must be active.
chk "release: image set from build/release-images.txt" "$REL" 'build/release-images\.txt'
[ -f "$ROOT/build/release-images.txt" ] \
  || { echo "FAIL: build/release-images.txt missing"; fails=1; }
active_imgs="$(grep -vE '^[[:space:]]*(#|$)' "$ROOT/build/release-images.txt")"
printf '%s\n' "$active_imgs" | grep -qx 'linux-base' \
  || { echo "FAIL: linux-base must be in the release set"; fails=1; }
printf '%s\n' "$active_imgs" | grep -qx 'linux-desktop' \
  || { echo "FAIL: linux-desktop must be in the release set"; fails=1; }
printf '%s\n' "$active_imgs" | grep -qx 'browser' \
  || { echo "FAIL: browser must be in the release set"; fails=1; }
for profile in browser linux-desktop; do
  if printf '%s\n' "$active_imgs" | grep -qx "$profile" \
    && ! printf '%s\n' "$active_imgs" | grep -qx 'linux-base'; then
    echo "FAIL: $profile requires linux-base (its FROM base) in the release set"
    fails=1
  fi
  # Both profiles must FROM the shared base (ARG BASE_IMAGE), never each other.
  grep -qE '^ARG BASE_IMAGE=ghcr\.io/tinyorbitvn/tinycdi-linux-base:' "$ROOT/build/$profile/Dockerfile" \
    || { echo "FAIL: build/$profile/Dockerfile must default BASE_IMAGE to tinycdi-linux-base"; fails=1; }
done

# v0.2 three-component platform: backend (public API + session gateway in
# one binary), frontend (static SPA server) and operator. The removed
# api/gateway/portal images and commands must not linger anywhere in the
# pipeline, and every image list must agree with build/release-images.txt.
for want in backend frontend operator; do
  printf '%s\n' "$active_imgs" | grep -qx "$want" \
    || { echo "FAIL: $want must be in the release set"; fails=1; }
done
for gone in api gateway portal; do
  printf '%s\n' "$active_imgs" | grep -qx "$gone" \
    && { echo "FAIL: removed image '$gone' is still in the release set"; fails=1; }
  for f in "$REL" "$IMG" "$ROOT/.github/scripts/"*.sh; do
    grep -qE "tinycdi-$gone\b|'$gone'|\b$gone\.ref|cmd/$gone\b|build/$gone/" "$f" \
      && { echo "FAIL: removed component '$gone' referenced in ${f#"$ROOT"/}"; fails=1; }
  done
done
for i in $active_imgs; do
  [ -f "$ROOT/build/$i/Dockerfile" ] \
    || { echo "FAIL: release image '$i' has no build/$i/Dockerfile"; fails=1; }
  # Every release image gets its own (var-gated) provenance attestation.
  grep -qE "attest build provenance — $i\$" "$REL" \
    || { echo "FAIL: release: no provenance attestation step for '$i'"; fails=1; }
done
# images.yml: build matrix (+ the dedicated base job) == scan matrix ==
# promote download/validate lists == the release set.
want_set="$(printf '%s\n' "$active_imgs" | sort | xargs)"
build_set="$( { grep -E '^        image: \[' "$IMG" | head -1 | tr -d '[] ' \
  | sed 's/^image://' | tr ',' '\n'; echo linux-base; } | sort | xargs)"
scan_set="$(grep -E '^        image: \[' "$IMG" | sed -n 2p | tr -d '[] ' \
  | sed 's/^image://' | tr ',' '\n' | sort | xargs)"
[ "$build_set" = "$want_set" ] \
  || { echo "FAIL: images: build matrix + base [$build_set] != release set [$want_set]"; fails=1; }
[ "$scan_set" = "$want_set" ] \
  || { echo "FAIL: images: scan matrix [$scan_set] != release set [$want_set]"; fails=1; }
while IFS= read -r line; do
  got="$(sed -E 's/.*(for img in|refs)[[:space:]]*//; s/; do.*//' <<< "$line" \
    | tr ' ' '\n' | grep -v '^\\$' | grep . | sort | xargs)"
  [ "$got" = "$want_set" ] \
    || { echo "FAIL: images: promote list [$got] != release set [$want_set]"; fails=1; }
done < <(grep -E 'for img in linux-base|validate-image-refs\.sh refs' -A1 "$IMG" \
  | grep -E 'for img in|^ +linux-base ')
# release.yml binaries: exactly the Go commands that ship.
bins="$(grep -E '^ +for comp in ' "$REL" | sed -E 's/.*for comp in //; s/; do//' | tr ' ' '\n' | sort | xargs)"
[ "$bins" = "backend operator" ] \
  || { echo "FAIL: release: static binaries [$bins] != [backend operator]"; fails=1; }
for c in $bins; do
  [ -d "$ROOT/cmd/$c" ] || { echo "FAIL: release binary '$c' has no cmd/$c"; fails=1; }
done
# Chart values carry a digest-stamping path per release image (SEC-18
# stamp): images.<key> for component/runtime images, kasmAdapter.image for
# the adapter init image — mirror of collect-publish-inputs.sh path_for.
VALS="$ROOT/deploy/helm/tinycdi/values.yaml"
for i in $active_imgs; do
  sec=images; key="$i"
  [ "$i" = "linux-base" ] && key=linuxBase
  [ "$i" = "linux-desktop" ] && key=linuxDesktop
  if [ "$i" = "kasm-adapter" ]; then sec=kasmAdapter; key=image; fi
  awk -v s="$sec" -v k="$key" \
    '/^[^ #]/ {in_s = ($0 ~ ("^" s ":"))} in_s && $0 ~ "^  " k ":" {f=1} END {exit !f}' "$VALS" \
    || { echo "FAIL: chart values lack $sec.$key for release image '$i'"; fails=1; }
done

# SUPR-7: the lint job pins a pathspec compatible with yamllint>=1.38.
chk "ci: compatible pathspec pin" "$CI" 'pathspec==1\.'
chk_absent "ci: conflicting pathspec pin" "$CI" 'pathspec==0\.12'

# SUPR-4 + D27/D28: the browser-engine freshness check and the runtime
# image age SLO moved from ci.yml to the daily runtime-freshness train.
chk "runtime-freshness: browser freshness check (chromium + firefox-esr)" "$FRESH" 'check-browser-freshness\.sh'
chk "runtime-freshness: pin-bump script wired" "$FRESH" 'bump-browser-pin\.sh'
chk "runtime-freshness: firefox-esr bumped too" "$FRESH" 'FIREFOX_ESR_APT_VERSION'
chk "runtime-freshness: run fails while a pin is stale" "$FRESH" 'fail the run while a pin is stale'
chk "runtime-freshness: image age check" "$FRESH" 'check-runtime-image-age\.sh'
chk "runtime-freshness: daily schedule" "$FRESH" 'cron: ".* \* \* \*"'
chk_absent "ci: freshness job moved out" "$CI" 'check-(browser|chromium)-freshness\.sh'
chk "runtime-images: manifest writer wired" "$TRAIN" 'write-runtime-manifest\.sh'
chk "runtime-images: rt tag only (no :main promotion)" "$TRAIN" 'imagetools create.*RT_TAG'
chk_absent "runtime-images: no :main/:latest promotion" "$TRAIN" 'imagetools create .*:(main|latest)'
chk "runtime-images: runtime-* release" "$TRAIN" 'gh release (create|upload) "\$REL_TAG"'
chk "runtime-images: runtime release never takes the repo Latest marker (v* owns it)" "$TRAIN" -- "--latest=false"
chk "runtime-images: never builds control-plane images" "$TRAIN" 'linux-desktop'
chk "runtime-images: builds the shared base" "$TRAIN" 'build/linux-base/\*\*'
chk "runtime-images: base is in the scan matrix" "$TRAIN" 'image: \[linux-base, linux-desktop, browser\]'
chk "runtime-images: manifest reads the desktop firefox pin" "$TRAIN" 'DESKTOP_DOCKERFILE: build/linux-desktop/Dockerfile'
# V3.26: the desktop and browser images must carry the SAME firefox-esr pin.
ff_d="$(awk -F= '$1 == "ARG FIREFOX_ESR_APT_VERSION" {print $2; exit}' "$ROOT/build/linux-desktop/Dockerfile")"
ff_b="$(awk -F= '$1 == "ARG FIREFOX_ESR_APT_VERSION" {print $2; exit}' "$ROOT/build/browser/Dockerfile")"
{ [ -n "$ff_d" ] && [ "$ff_d" = "$ff_b" ]; } \
  || { echo "FAIL: firefox-esr pin differs between linux-desktop ('$ff_d') and browser ('$ff_b')"; fails=1; }
chk_absent "runtime-images: no api/backend/gateway build" "$TRAIN" 'build/(api|backend|operator|gateway|portal|frontend)/Dockerfile'
for s in check-browser-freshness.sh bump-browser-pin.sh; do
  [ -x "$ROOT/.github/scripts/$s" ] \
    || { echo "FAIL: $s missing/not executable"; fails=1; }
done

# SUPR-5 + SUPF-2/3/4: post-public protection script exists, defaults to
# dry-run, grants the Actions app no tag bypass, requires 0 reviews while
# there is a single maintainer, keeps the network freshness check
# non-required, and enables PVR + secret scanning. Full policy rendering
# is asserted by .github/tests/setup-repo-protection.test.sh.
PROT="$ROOT/.github/scripts/setup-repo-protection.sh"
[ -x "$PROT" ] || { echo "FAIL: setup-repo-protection.sh missing/not executable"; fails=1; }
chk "protection: release environment" "$PROT" 'environments/release'
chk "protection: tag ruleset" "$PROT" 'rulesets'
chk "protection: branch protection" "$PROT" 'branches/main/protection'
chk_absent "protection: no Actions app bypass" "$PROT" '15368|Integration'
chk "protection: single-maintainer review count" "$PROT" 'required_approving_review_count": *0'
chk_absent "protection: freshness check not required" "$PROT" 'chromium apt-pin freshness'
chk "protection: private vuln reporting" "$PROT" 'private-vulnerability-reporting'
chk "protection: secret scanning + push protection" "$PROT" 'secret_scanning_push_protection'

# PUB-2: KasmVNC corresponding-source bundle is built and shipped as a
# release asset; runtime images carry the SPDX license expression + a
# source-offer pointer label.
chk "release: kasmvnc source bundle step" "$REL" 'kasmvnc-source-bundle\.sh'
chk "release: runtime image SPDX licenses" "$REL" 'licenses=MIT AND GPL-2\.0-only'
chk "images: runtime image SPDX licenses" "$IMG" 'licenses=MIT AND GPL-2\.0-only'
chk "release: source-offer pointer label" "$REL" 'kasmvnc-source-offer='
chk "images: source-offer pointer label" "$IMG" 'kasmvnc-source-offer='
chk "release: kasmvnc files in asset allowlist" "$ROOT/.github/scripts/collect-publish-inputs.sh" \
  'corresponding-source\.tar\.gz'

# PUB-4: no internal registry name in .github; the optional base mirror is
# generic via vars.BASE_MIRROR_REGISTRY + BASE_MIRROR_* secrets.
for f in "$REL" "$IMG" "$CI" "$ROOT/.github/README.md" "$ROOT/.github/scripts/"*.sh; do
  grep -q 'crhub\.tinyorbit\.vn\|CRHUB_' "$f" \
    && { echo "FAIL: internal registry reference in $f"; fails=1; }
done
chk "release: generic base mirror var" "$REL" 'vars\.BASE_MIRROR_REGISTRY'
chk "images: generic base mirror var" "$IMG" 'vars\.BASE_MIRROR_REGISTRY'

# SEC-43: version validated via the anchored-regex script, not grep.
chk "release: version via validation script" "$REL" 'validate-release-version.sh'
chk_absent "release: no line-based version grep" "$REL" 'grep -qE.*\^v\[0-9\]'

# SEC-05 mirrors for images.yml.
chk "images: digest-only push" "$IMG" 'push-by-digest=true'
chk_absent "images: no trivy-action" "$IMG" 'uses:.*trivy-action'
chk "images: scan job is packages:read" "$IMG" 'packages: read'
chk "images: publish only on main ref" "$IMG" "github.ref == 'refs/heads/main'"
chk_absent "images: no job-level registry secrets" "$IMG" '^ {2,6}(CRHUB|BASE_MIRROR)_[A-Z]+:'

# SEC-44: expiring-exceptions file and its policy check exist.
[ -f "$ROOT/.trivyignore" ] || { echo "FAIL: .trivyignore missing"; fails=1; }
[ -x "$ROOT/.github/scripts/check-trivyignore.sh" ] || { echo "FAIL: check-trivyignore.sh missing"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "release-topology: all guards pass"; fi
exit "$fails"
