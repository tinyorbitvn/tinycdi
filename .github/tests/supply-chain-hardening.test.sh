#!/usr/bin/env bash
# supply-chain-hardening.test.sh — structural guards for the v1.0
# supply-chain hardening pass on the publishing workflows:
#
#   * scan-env hygiene (SUPF-10): every scan job must validate the
#     artifact-supplied image ref against the strict
#     ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<64hex> regex BEFORE it
#     lands in $GITHUB_ENV — a newline in a forged ref file would
#     otherwise inject arbitrary env vars into the scan job.
#   * runtime manifest signing (SEC-17): runtime-images.json is the
#     deployment contract consumers pin digests from — it ships with a
#     cosign sign-blob Sigstore bundle on the runtime-* release, signed
#     in the same job that publishes it.
#   * train provenance parity: the runtime train's publish job carries
#     the same var-gated actions/attest-build-provenance legs as
#     release.yml — gated on its own TRAIN_ATTESTATIONS_ENABLED variable
#     (defaults OFF until verified), with attestations:write scoped to
#     the publish job only.
#   * release-asset SBOMs (SEC-17): the packaged Helm chart and the
#     release binaries get dedicated syft SBOMs, validated by
#     collect-publish-inputs.sh and signed/released like every other
#     asset.
#   * digest-addressability is documented: gate-failed digest-pushes
#     stay pullable-by-digest but are never tagged/signed — the docs
#     must say so.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMG="$ROOT/.github/workflows/images.yml"
REL="$ROOT/.github/workflows/release.yml"
TRAIN="$ROOT/.github/workflows/runtime-images.yml"
COLLECT="$ROOT/.github/scripts/collect-publish-inputs.sh"
README="$ROOT/.github/README.md"
PROV="$ROOT/docs/security/provenance.md"
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

# ---------------------------------------------------------------
# SUPF-10: scan-env hygiene — the ref is validated BEFORE the
# $GITHUB_ENV write, inside the same 'resolve scan target' step.
# ---------------------------------------------------------------
for wf in "$IMG" "$REL" "$TRAIN"; do
  name="$(basename "$wf")"
  block="$(awk '
    /^      - name: resolve scan target/ { inb=1 }
    inb && /^      - / && !/resolve scan target/ { inb=0 }
    inb { print }
  ' "$wf")"
  [ -n "$block" ] || { echo "FAIL: $name: no 'resolve scan target' step"; fails=1; continue; }
  grep -qF 'SCAN_REF=' <<< "$block" \
    || { echo "FAIL: $name: resolve step does not write SCAN_REF"; fails=1; }
  grep -qF 'ghcr\.io/tinyorbitvn/tinycdi-${IMG}@sha256:[0-9a-f]{64}' <<< "$block" \
    || { echo "FAIL: $name: resolve step lacks the strict ref regex"; fails=1; }
  # the guard must precede the env write
  la="$(grep -nF 'tinycdi-${IMG}@sha256' <<< "$block" | head -1 | cut -d: -f1)"
  lb="$(grep -nF 'SCAN_REF=' <<< "$block" | head -1 | cut -d: -f1)"
  { [ -n "$la" ] && [ -n "$lb" ] && [ "$la" -lt "$lb" ]; } \
    || { echo "FAIL: $name: ref regex is not evaluated before the SCAN_REF env write"; fails=1; }
  # a non-conforming ref must fail the step, not fall through
  grep -qE 'exit 1' <<< "$block" \
    || { echo "FAIL: $name: invalid ref does not fail the scan job"; fails=1; }
done

# Unit-test the guard regex itself — the same pattern the scan steps run.
IMG=browser
accept() { [[ "$1" =~ ^ghcr\.io/tinyorbitvn/tinycdi-${IMG}@sha256:[0-9a-f]{64}$ ]]; }
D64="$(printf 'a%.0s' $(seq 64))"
accept "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$D64" \
  || { echo "FAIL: regex rejects a valid ref"; fails=1; }
for bad in \
  "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$D64
POISONED=1" \
  "ghcr.io/evil/tinycdi-browser@sha256:$D64" \
  "ghcr.io/tinyorbitvn/tinycdi-backend@sha256:$D64" \
  "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$(printf 'a%.0s' $(seq 63))" \
  "ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$(printf 'A%.0s' $(seq 64))" \
  "ghcr.io/tinyorbitvn/tinycdi-browser:latest" \
  " ghcr.io/tinyorbitvn/tinycdi-browser@sha256:$D64" \
  "local"; do
  if accept "$bad"; then
    echo "FAIL: regex accepted $(printf '%q' "$bad")"; fails=1
  fi
done

# ---------------------------------------------------------------
# SEC-17: runtime-images.json is sign-blob'd by the publish job and
# the bundle rides the runtime-* release next to the manifest.
# ---------------------------------------------------------------
chk "runtime-images: sign-blob the manifest" "$TRAIN" 'cosign sign-blob .*runtime-images\.json'
chk "runtime-images: sigstore bundle for the manifest" "$TRAIN" 'runtime-images\.json\.sigstore\.json'
chk "runtime-images: bundle uploaded to the release" "$TRAIN" 'gh release (upload|create).*runtime-images\.json\.sigstore\.json|runtime-images\.json runtime-images\.json\.sigstore\.json'
order "runtime-images: manifest signed before the release write" "$TRAIN" \
  'cosign sign-blob' 'create or update the runtime release'

# the consumption docs carry the exact verify-blob line with the
# pinned signer identity (workflow path + refs/heads/main)
chk "docs: README documents manifest verify-blob" "$README" \
  'cosign verify-blob .*runtime-images\.json'
chk "docs: README pins the train signer identity" "$README" \
  'runtime-images\.yml@refs/heads/main'
chk "docs: images.md points at the signed manifest" "$ROOT/docs/images.md" \
  'verify-blob|sigstore\.json'

# ---------------------------------------------------------------
# Train build-provenance parity with release.yml — var-gated OFF by
# default (TRAIN_ATTESTATIONS_ENABLED), attestations:write only on
# the publish job.
# ---------------------------------------------------------------
PUB="$(sed -n '/^  publish:/,$p' "$TRAIN")"
chk "runtime-images: publish job holds attestations: write" <(echo "$PUB") \
  'attestations: write'
n="$(grep -c 'attestations: write' "$TRAIN")"
[ "$n" -eq 1 ] \
  || { echo "FAIL: runtime-images: attestations: write appears $n times (want exactly the publish job)"; fails=1; }
chk "runtime-images: attestations gated on TRAIN_ATTESTATIONS_ENABLED" "$TRAIN" \
  "TRAIN_ATTESTATIONS_ENABLED == 'true'"
chk "runtime-images: sha-pinned attest-build-provenance" "$TRAIN" \
  'actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8'
for i in linux-base linux-desktop browser; do
  chk "runtime-images: provenance attestation for $i" "$TRAIN" \
    "attest build provenance — $i"
done
# each attest step is var-gated: three ifs + the subject-resolver
n="$(grep -c "TRAIN_ATTESTATIONS_ENABLED == 'true'" "$TRAIN")"
[ "$n" -ge 4 ] \
  || { echo "FAIL: runtime-images: only $n TRAIN_ATTESTATIONS_ENABLED gates (want resolver + 3 legs)"; fails=1; }
order "runtime-images: subjects resolved before attestation" "$TRAIN" \
  'resolve attestation subjects' 'attest build provenance — linux-base'
order "runtime-images: attestation before rt tag promotion" "$TRAIN" \
  'attest build provenance — linux-base' 'name: promote rt tag'
chk "docs: README documents TRAIN_ATTESTATIONS_ENABLED" "$README" \
  'TRAIN_ATTESTATIONS_ENABLED'

# ---------------------------------------------------------------
# SEC-17: dedicated SBOMs for the packaged chart and the release
# binaries — built by the producing job, validated by
# collect-publish-inputs.sh, shipped + signed in the bundle.
# ---------------------------------------------------------------
chk "release: chart SBOM step" "$REL" 'sbom-chart\.spdx\.json'
chk "release: chart SBOM artifact" "$REL" 'name: sbom-chart'
chk "release: binaries SBOM step" "$REL" 'sbom-binaries\.spdx\.json'
chk "release: binaries SBOM artifact" "$REL" 'name: sbom-binaries'
chk "release: publish downloads sbom-chart" "$REL" 'gh run download.*-n sbom-chart'
chk "release: publish downloads sbom-binaries" "$REL" 'gh run download.*-n sbom-binaries'
chk "collect: validates sbom-chart" "$COLLECT" 'sbom-chart'
chk "collect: validates sbom-binaries" "$COLLECT" 'sbom-binaries'
chk "collect: extra SBOMs join the signed bundle" "$COLLECT" \
  'cp .*sboms/.*\.spdx\.json.*bundle'

# ---------------------------------------------------------------
# F3 docs: digest-addressable gate-failed manifests are documented.
# ---------------------------------------------------------------
chk "docs: digest-addressable != released documented" "$PROV" \
  'pullable|digest-addressable'
chk "docs: unsigned digest fails cosign verify" "$PROV" \
  'never (signed|tagged)|unsigned'

[ "$fails" -eq 0 ] && echo "supply-chain-hardening: all guards pass"
exit "$fails"
