#!/usr/bin/env bash
# cleanup-failed-digests.sh — delete exactly the GHCR package versions a
# failed publishing run pushed (SR-5-F3).
#
# Build jobs push `type=registry,push-by-digest=true` BEFORE the trivy
# gate; a gate-failed (or aborted) publish run used to leave those
# unsigned manifests pullable-by-digest forever. The publishing
# workflows' `cleanup` job feeds this script the run's own
# image-ref artifacts and it deletes precisely the versions that run
# created:
#
#   * the pushed index digest itself;
#   * every manifest listed in that index — the per-platform child
#     manifests and the buildkit attestation-manifest entries;
#   * every referrer the registry records for the pushed digest, plus
#     cosign's sha256-<digest>.{sig,att,sbom} tag-derived versions.
#
# It is strictly RUN-SCOPED — never a blanket untagged sweep. GHCR keeps
# a published image's multi-arch children and its cosign sha256-*
# signature/attestation manifests as versions of the same package that
# carry no release tag (or only digest-derived ones); pruning "untagged"
# versions by age or count would delete the legs of published images.
# The only versions deleted are ones provably part of this run's push.
#
# Fail closed: any targeted version that carries a tag which is not
# digest-derived — i.e. not ^sha256-<64hex>[.<suffix>] — is REFUSED
# (a real tag means the content was promoted and is no longer this
# run's residue) and the script exits non-zero.
#
# Usage: cleanup-failed-digests.sh <refs-dir>
#   <refs-dir> holds this run's <image>.ref artifacts; each contains
#   either the literal "local" (a non-publishing leg) or
#   ghcr.io/tinyorbitvn/tinycdi-<image>@sha256:<64hex>.
#
# Env:  GH_TOKEN       — workflow GITHUB_TOKEN; the job needs
#                        `packages: write` (delete + version listing).
#       GITHUB_ACTOR  — ghcr token-endpoint username (optional).
# Exit: 0 clean, 1 any refusal/API failure/malformed ref, 2 usage.
set -uo pipefail

[ "${1:-}" = "" ] && { echo "usage: $0 <refs-dir>" >&2; exit 2; }
REFS="$1"
[ -d "$REFS" ] || { echo "::error::refs dir '$REFS' not found"; exit 2; }
[ -n "${GH_TOKEN:-}" ] || { echo "::error::GH_TOKEN is required"; exit 2; }

ORG=tinyorbitvn
REF_RE='^ghcr\.io/tinyorbitvn/(tinycdi-[a-z0-9-]+)@sha256:([0-9a-f]{64})$'
# A tag a deletable version may carry: digest-derived only — the bare
# sha256-<hex> form GHCR uses for digest pushes and cosign's
# sha256-<hex>.{sig,att,sbom,...} convention. Anything else (a release
# tag) means the content is promoted — refuse.
TAG_OK_RE='^sha256-[0-9a-f]{64}(\.[0-9a-z-]+)?$'

status=0

ghcr_token() { # $1 package — pull-scope bearer token for the registry API
  curl -fsSL -u "${GITHUB_ACTOR:-x}:$GH_TOKEN" \
    "https://ghcr.io/token?scope=repository:$1:pull" | jq -r '.token // empty'
}

manifest_children() { # $1 pkg $2 digest — child digests of an index/manifest
  local tok
  tok="$(ghcr_token "$1")" || return 1
  [ -n "$tok" ] || return 1
  curl -fsSL -H "Authorization: Bearer $tok" \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/$1/manifests/$2" | jq -r '.manifests[]?.digest'
}

referrers() { # $1 pkg $2 digest — referrer digests; empty when none/unsupported
  local tok
  tok="$(ghcr_token "$1")" || return 0
  [ -n "$tok" ] || return 0
  curl -fsSL -H "Authorization: Bearer $tok" \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    "https://ghcr.io/v2/$1/referrers/$2" 2>/dev/null \
    | jq -r '.manifests[]?.digest' || true
}

versions_tsv() { # $1 pkg — "id<TAB>manifest-digest<TAB>comma-tags" per version
  gh api --paginate "orgs/$ORG/packages/container/$1/versions?per_page=100" \
    | jq -r '.[] | [.id, .name,
                   ((.metadata.container.tags // []) | join(","))] | @tsv'
}

clean_package() { # $1 pkg $2 pushed index digest
  local pkg="$1" idx="$2" hex="${2#sha256:}"
  local -A set=([$2]=1) seen=()
  local d children refs rc=0
  local -a idx_ids=() other_ids=() tags=()

  # Index -> children. Without the manifest we cannot name the children,
  # and deleting only the index while its legs linger is useless — skip
  # the package instead (residual = pre-fix behaviour, never corruption).
  if ! children="$(manifest_children "$pkg" "$idx")"; then
    echo "::error::$pkg@$idx: manifest fetch failed — skipping package"
    return 1
  fi
  while IFS= read -r d; do
    [ -n "$d" ] && set["$d"]=1
  done <<< "$children"

  # OCI referrers (cosign signatures/attestations attached to the digest);
  # registries without the API return nothing.
  refs="$(referrers "$pkg" "$idx")"
  while IFS= read -r d; do
    [ -n "$d" ] && set["$d"]=1
  done <<< "$refs"

  local rows
  if ! rows="$(versions_tsv "$pkg")"; then
    echo "::error::$pkg: cannot list package versions — skipping"
    return 1
  fi

  local vid vname vtags t target is_index bad
  while IFS=$'\t' read -r vid vname vtags; do
    target="" is_index=""
    if [ -n "${set[$vname]:-}" ]; then
      target=1; seen[$vname]=1
      [ "$vname" = "$idx" ] && is_index=1
    else
      # tag derived from this run's index digest: sha256-<hex>[.<sfx>]
      IFS=',' read -ra tags <<< "$vtags"
      for t in ${tags[@]+"${tags[@]}"}; do
        [[ "$t" =~ ^sha256-${hex}(\.[0-9a-z-]+)?$ ]] && { target=1; break; }
      done
    fi
    [ -n "$target" ] || continue

    # fail closed on any non-digest tag — promoted content is never
    # this run's residue.
    bad=""
    IFS=',' read -ra tags <<< "$vtags"
    for t in ${tags[@]+"${tags[@]}"}; do
      [ -z "$t" ] && continue
      [[ "$t" =~ $TAG_OK_RE ]] || bad+="$t "
    done
    if [ -n "$bad" ]; then
      echo "::error::$pkg version $vid ($vname) carries non-digest tag(s): ${bad% } — refusing to delete"
      rc=1
      continue
    fi
    if [ -n "$is_index" ]; then idx_ids+=("$vid"); else other_ids+=("$vid"); fi
  done <<< "$rows"

  # children/referrers first, the index version last.
  for vid in ${other_ids[@]+"${other_ids[@]}"} ${idx_ids[@]+"${idx_ids[@]}"}; do
    [ -n "$vid" ] || continue
    if gh api -X DELETE "orgs/$ORG/packages/container/$pkg/versions/$vid" >/dev/null; then
      echo "deleted $pkg version $vid"
    else
      echo "::error::delete failed for $pkg version $vid"
      rc=1
    fi
  done

  for d in "${!set[@]}"; do
    [ -n "${seen[$d]:-}" ] \
      || echo "::warning::$pkg@$d: pushed this run but matched no package version (registry lag?)"
  done
  return "$rc"
}

shopt -s nullglob
files=("$REFS"/*.ref)
[ "${#files[@]}" -gt 0 ] || { echo "::notice::no image refs — nothing to clean"; exit 0; }

for f in "${files[@]}"; do
  ref="$(tr -d '[:space:]' < "$f")"
  if [ "$ref" = "local" ]; then
    echo "$(basename "$f"): non-publishing leg — skip"
    continue
  fi
  if [[ "$ref" =~ $REF_RE ]]; then
    clean_package "${BASH_REMATCH[1]}" "sha256:${BASH_REMATCH[2]}" || status=1
  else
    echo "::error::$f is not ghcr.io/$ORG/tinycdi-<img>@sha256:<64hex> ('$ref')"
    status=1
  fi
done
exit "$status"
