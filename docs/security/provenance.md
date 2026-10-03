# Release provenance — SBOMs, signatures and attestations

Status: **implemented in CI**. The pipeline in `.github/workflows/images.yml`
(every `main` push) and `.github/workflows/release.yml` (`v*` tags) produces
signed, attested artifacts as described below — tools are sha256-pinned
(syft 1.52.0, trivy 0.70.0, cosign v3.1.3). No release tag has been cut yet;
this documents the mechanism a `vX.Y.Z` release exercises.

## What a release artifact consists of

Per the release interface contract, a release artifact has: image digests,
SBOMs, provenance, NOTICE, and the compatibility matrix
(`docs/compatibility.md`). Concretely, for each shipped image
(`backend`, `frontend`, `operator`, `linux-base`, `linux-desktop`, `browser`, `kasm-adapter`) plus
the Helm chart:

1. **Digest-pinned reference** — build jobs push **by digest only**
   (`push-by-digest=true`); `imagetools create` promotes tags (`:sha-<short>`,
   `:main`, `:X.Y.Z`) only after signing (SUPR-13). The digest lands in the
   release manifest and in the Helm values a release deploys with
   (`images.*.digest` in `deploy/helm/tinycdi/values.yaml`; seeded
   `templates[]` image refs must be digest-pinned or CRD validation rejects
   them).
2. **SBOM** — SPDX JSON generated with pinned syft in the **build job**
   (the scan and promote jobs never consume scan-job outputs — SUPR-2).
   Release assets additionally carry `sbom-<image>.spdx.json` files.
3. **Vulnerability report** — trivy SARIF + table per image/arch, produced
   by the no-secrets scan job; the gate semantics live in
   [vulnerability-policy.md](vulnerability-policy.md).
4. **Signature + attestation** — see below.
5. **License inventory** — NOTICE + `THIRD_PARTY_LICENSES.md` regenerated
   at the release ref.

## Signing and attestation flow (as built)

Signing identity: **keyless cosign via GitHub Actions OIDC**
(`id-token: write`) → Fulcio-issued certificate bound to the exact workflow
identity, recorded in the Rekor transparency log. No signing keys are
stored anywhere. Per digest:

```sh
cosign sign --yes ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<digest>
cosign attest --yes --type spdxjson \
  --predicate sboms/sbom-<img>.spdx.json \
  ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<digest>
```

The signer certificate identity is exact (SUPR-6 — anchored, no regex
ambiguity):

| Channel | Workflow | Identity |
|---|---|---|
| `:main` / `:sha-*` images | images.yml | `https://github.com/tinyorbitvn/tinycdi/.github/workflows/images.yml@refs/heads/main` |
| release images + chart + blobs | release.yml | `https://github.com/tinyorbitvn/tinycdi/.github/workflows/release.yml@refs/tags/v<X.Y.Z>` |

Release runs additionally `cosign sign` the pushed OCI chart digest
(`oci://ghcr.io/tinyorbitvn/charts/tinycdi`) and `cosign sign-blob` every
release asset (`<asset>.sigstore.json` bundles), then `gh release create`.
GitHub build-provenance attestations (`actions/attest-build-provenance`)
are emitted only when the `ATTESTATIONS_ENABLED` repo variable is set —
attestation storage on private repos requires GitHub Enterprise; the cosign
SBOM attestations attached to the digests are unaffected.

The provenance predicate records, at minimum:

| Field | Source of truth |
|---|---|
| Source tree state | the git SHA (`GITHUB_SHA`) + tag; the release refuses refs not on `main` ancestry and refuses to overwrite an existing tag/release/GHCR version |
| Build inputs | `FROM` digests (`golang`, `distroless/static`, `debian:bookworm-slim` lock entries), `ARG BASE_IMAGE` (the linux-desktop and browser profiles build FROM the exact pushed linux-base digest), pinned tool tarballs |
| Build recipe | the workflow jobs themselves — pinned actions + sha256-verified tools |
| Builder identity | the GitHub Actions OIDC identity (exact certificate identity above) |

## Verification side (what a consumer runs)

The full, copy-pasteable command set lives in `.github/README.md`
"Verifying a release". Shapes:

```sh
cosign verify ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity '<exact workflow identity above>'

cosign verify-attestation --type spdxjson \
  ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity '<exact workflow identity above>'

# release assets:
cosign verify-blob --bundle <asset>.sigstore.json <asset> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity 'https://github.com/tinyorbitvn/tinycdi/.github/workflows/release.yml@refs/tags/vX.Y.Z'
```

Deployment-side enforcement (optional, later): a policy check that the
cluster only runs digests carrying a valid attestation — admission-level
verification is out of MVP scope; the Helm chart already refuses
non-digest image references in seeded templates and requires a pinned tag
or digest for the node-profiles installer image, which is the MVP-level
guarantee.
