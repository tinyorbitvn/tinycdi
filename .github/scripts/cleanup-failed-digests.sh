#!/usr/bin/env bash
# cleanup-failed-digests.sh — delete exactly the GHCR package versions a
# failed publishing run pushed (SR-5-F3).
#
# Build jobs push `type=registry,push-by-digest=true` BEFORE the trivy
# gate; a gate-failed publish run used to leave those
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
# Deletion is bounded twice — a version goes only when BOTH hold:
#
#   (a) it was created within this run's window (created_at >=
#       RUN_STARTED_AT). GHCR dedups manifest digests — an identical
#       rebuild re-pushes the same digest and the SAME version record
#       answers, so a pre-existing version can never be this run's
#       residue. This also stops a forged ref or an identical rebuild
#       from deleting a published digest.
#   (b) it is reachable ONLY from this run's tree — never from any
#       tagged manifest in the package. For every version carrying a
#       non-digest tag the script resolves its index children and its
#       cosign sha256-<hex>.* artifacts and treats the whole reachable
#       set as published. A failed run whose fresh index happens to
#       share a child with a published image must not delete that
#       child.
#
# Whole-tree refusal: if ANY candidate version carries a non-digest tag
# or is reachable from a tagged manifest, NOTHING is deleted for that
# image — the refusal is logged and is not a job failure (an identical
# rebuild or a leg pointing at published content is an expected
# refuse-path, not an error).
#
# It is strictly RUN-SCOPED — never a blanket unlabelled sweep. GHCR
# keeps a published image's multi-arch children and its cosign
# sha256-* signature/attestation manifests as versions of the same
# package that carry no release tag (or only digest-derived ones);
# pruning "unlabelled" versions by age or count would delete the legs
# of published images.
#
# Usage: cleanup-failed-digests.sh <refs-dir>
#   <refs-dir> holds this run's <image>.ref artifacts; each contains
#   either the literal "local" (a non-publishing leg) or
#   ghcr.io/tinyorbitvn/tinycdi-<image>@sha256:<64hex>.
#
# Env:  GH_TOKEN        — workflow GITHUB_TOKEN; the job needs
#                         `packages: write` (delete + version listing).
#       RUN_STARTED_AT  — ISO 8601 start of THIS run (the run's
#                         created_at window bound); required.
#       GITHUB_ACTOR   — ghcr token-endpoint username (optional).
# Exit: 0 when every image is cleaned or safely refused, 1 on API
#       errors / malformed refs, 2 usage.
set -uo pipefail

[ "${1:-}" = "" ] && { echo "usage: $0 <refs-dir>" >&2; exit 2; }
REFS="$1"
[ -d "$REFS" ] || { echo "::error::refs dir '$REFS' not found"; exit 2; }
[ -n "${GH_TOKEN:-}" ] || { echo "::error::GH_TOKEN is required"; exit 2; }
[ -n "${RUN_STARTED_AT:-}" ] \
  || { echo "::error::RUN_STARTED_AT is required (the run window bound)"; exit 2; }

ORG=tinyorbitvn
REF_RE='^ghcr\.io/tinyorbitvn/(tinycdi-[a-z0-9-]+)@sha256:([0-9a-f]{64})$'
# A tag a deletable version may carry: digest-derived only — the bare
# sha256-<hex> form GHCR uses for digest pushes and cosign's
# sha256-<hex>.{sig,att,sbom,...} convention. Anything else (a release
# tag) means the content is promoted — refuse.
TAG_OK_RE='^sha256-[0-9a-f]{64}(\.[0-9a-z-]+)?$'

status=0

repo_path() { echo "$ORG/$1"; } # $1 package — namespaced repository
# path. GHCR keys both the token scope and every /v2/ route on
# <org>/<pkg>: a bare package name is rejected (400 NAME_INVALID on
# /v2/, 400/403 on the token endpoint). Every call goes through this
# one helper so the two surfaces cannot drift apart again.

ghcr_token() { # $1 package — pull-scope bearer token for the registry
  # API. The scope MUST carry the org namespace — repository:<pkg>:pull
  # is rejected by the token endpoint (400) and its token cannot pull
  # the namespaced repository (403 on every manifest/referrer GET).
  curl -fsSL -u "${GITHUB_ACTOR:-x}:$GH_TOKEN" \
    "https://ghcr.io/token?scope=repository:$(repo_path "$1"):pull" | jq -r '.token // empty'
}

manifest_json() { # $1 pkg $2 digest $3 bearer — raw manifest document
  curl -fsSL -H "Authorization: Bearer $3" \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/$(repo_path "$1")/manifests/$2"
}

MAX_DEPTH=4
tree_digests() { # $1 pkg $2 root digest $3 bearer — every descendant
  # digest, transitively: children of the root and of any nested index,
  # up to MAX_DEPTH levels. rc 0 ok, 1 manifest fetch error, 2 an index
  # is nested deeper than the bound (caller fails closed).
  local pkg="$1" tok="$3"
  local -A seen=()
  local -a stack=("$2:0")
  local item dg dep body d mt
  while [ "${#stack[@]}" -gt 0 ]; do
    item="${stack[-1]}"; unset 'stack[-1]'
    dg="${item%:*}"; dep="${item##*:}"   # sha256:<hex>:<depth>
    [ -n "${seen[$dg]:-}" ] && continue
    seen[$dg]=1
    body="$(manifest_json "$pkg" "$dg" "$tok")" || return 1
    while IFS='|' read -r d mt; do
      [ -z "$d" ] && continue
      echo "$d"
      case "$mt" in
        *index*|*manifest.list*)
          [ $((dep + 1)) -gt "$MAX_DEPTH" ] && return 2
          stack+=("$d:$((dep + 1))") ;;
      esac
    done <<< "$(jq -r '.manifests[]? | [.digest, (.mediaType // "")] | join("|")' <<< "$body")"
  done
}

referrers() { # $1 pkg $2 digest $3 bearer — referrer digests; rc 1 on
  # fetch error. GHCR answers 404 MANIFEST_UNKNOWN for a subject with
  # no referrers (its cosign sig/att live as sha256-* tags — already
  # covered by the tag-derived candidates), and the subject's
  # existence is proven by the manifest fetch that ran moments before
  # — so 404 means "empty", not "unknown". Any OTHER failure — 401,
  # 403, any other 4xx, 5xx, or a network error — means the tree
  # cannot be proven complete: the caller fails closed (whole-image
  # refusal) instead.
  local out code body
  out="$(curl -sS -H "Authorization: Bearer $3" \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    -w $'\n%{http_code}' \
    "https://ghcr.io/v2/$(repo_path "$1")/referrers/$2")" || return 1
  code="${out##*$'\n'}"
  body="${out%$'\n'*}"
  case "$code" in
    404) return 0 ;;
    200) ;;
    *) return 1 ;;
  esac
  jq -r '.manifests[]?.digest' <<< "$body"
}

versions_tsv() { # $1 pkg — "id|digest|comma-tags|created_at" per version.
  # `|` (not TAB): whitespace IFS collapses empty fields, and an empty
  # tag list must not shift created_at into the tags slot.
  gh api --paginate "orgs/$ORG/packages/container/$1/versions?per_page=100" \
    | jq -r '.[] | [.id, .name,
                   ((.metadata.container.tags // []) | join(",")),
                   (.created_at // "")] | join("|")'
}

clean_package() { # $1 pkg $2 pushed index digest
  local pkg="$1" idx="$2" hex="${2#sha256:}"
  local -A cand=([$2]=1) seen=() referenced=() taghex=()
  local tok children refs rows d vid vname vtags vc t kids tagged derived tree_rc
  local -a tags=() c_ids=() c_names=() c_created=() del_now=() del_idx=()
  local refuse="" rc=0 i

  tok="$(ghcr_token "$pkg")"
  [ -n "$tok" ] \
    || { echo "::error::$pkg: ghcr token fetch failed — skipping"; return 1; }

  # This run's tree: index -> children (per-platform + buildkit
  # attestation manifests, transitively — nested indexes up to
  # MAX_DEPTH) -> referrers. Without the index manifest we cannot name
  # the children — skip the package rather than delete only the index
  # and orphan its legs. An index nested deeper than the bound is a
  # fail-closed refusal, not an error.
  if children="$(tree_digests "$pkg" "$idx" "$tok")"; then
    tree_rc=0
  else
    tree_rc=$?
  fi
  case "$tree_rc" in
    0) ;;
    2) echo "::warning::$pkg@$idx: index nested deeper than $MAX_DEPTH — refusing to delete"
       return 0 ;;
    *) echo "::error::$pkg@$idx: manifest fetch failed — skipping package"
       return 1 ;;
  esac
  while IFS= read -r d; do [ -n "$d" ] && cand["$d"]=1; done <<< "$children"
  if ! refs="$(referrers "$pkg" "$idx" "$tok")"; then
    echo "::warning::$pkg@$idx: referrers fetch failed — refusing to delete (tree possibly incomplete)"
    return 0
  fi
  while IFS= read -r d; do [ -n "$d" ] && cand["$d"]=1; done <<< "$refs"

  if ! rows="$(versions_tsv "$pkg")"; then
    echo "::error::$pkg: cannot list package versions — skipping"
    return 1
  fi

  # Pass 1 — candidates and tagged manifests. A candidate is a version
  # whose manifest digest is in this run's tree or that carries a tag
  # derived from this run's index digest (sha256-<hex>[.<sfx>]). A
  # "tagged manifest" is any version with a non-digest tag — published
  # content whose reach must be protected.
  while IFS='|' read -r vid vname vtags vc; do
    tagged="" derived=""
    IFS=',' read -ra tags <<< "$vtags"
    for t in ${tags[@]+"${tags[@]}"}; do
      [ -z "$t" ] && continue
      [[ "$t" =~ $TAG_OK_RE ]] || tagged=1
      [[ "$t" =~ ^sha256-${hex}(\.[0-9a-z-]+)?$ ]] && derived=1
    done

    if [ -n "${cand[$vname]:-}" ] || [ -n "$derived" ]; then
      c_ids+=("$vid"); c_names+=("$vname"); c_created+=("$vc")
      [ -n "${cand[$vname]:-}" ] && seen[$vname]=1
      if [ -n "$tagged" ]; then
        refuse+="version $vid ($vname) carries non-digest tag(s): $vtags; "
      fi
    fi

    [ -n "$tagged" ] || continue
    taghex[${vname#sha256:}]=1
    referenced[$vname]=1
    # The tagged manifest's whole tree is published legs — never
    # deletable. An unreadable or over-deep nested index proves no
    # non-overlap — fail closed for the whole image.
    if kids="$(tree_digests "$pkg" "$vname" "$tok")"; then
      while IFS= read -r d; do [ -n "$d" ] && referenced[$d]=1; done <<< "$kids"
    else
      refuse+="cannot enumerate tagged manifest $vname (proving non-overlap impossible); "
    fi
  done <<< "$rows"

  # Pass 2 — cosign reachability: a version tagged sha256-<Thex>[.<sfx>]
  # is the signature/attestation artifact of tagged manifest T.
  while IFS='|' read -r vid vname vtags vc; do
    IFS=',' read -ra tags <<< "$vtags"
    for t in ${tags[@]+"${tags[@]}"}; do
      if [[ "$t" =~ ^sha256-([0-9a-f]{64})(\.[0-9a-z-]+)?$ ]] \
        && [ -n "${taghex[${BASH_REMATCH[1]}]:-}" ]; then
        referenced[$vname]=1
      fi
    done
  done <<< "$rows"

  # Whole-tree refusal — ANY overlap with published content cancels the
  # image's deletion entirely. Logged, never a job failure.
  for i in "${!c_ids[@]}"; do
    if [ -n "${referenced[${c_names[$i]}]:-}" ]; then
      refuse+="version ${c_ids[$i]} (${c_names[$i]}) is reachable from a tagged manifest; "
    fi
  done
  if [ -n "$refuse" ]; then
    echo "::warning::$pkg@$idx: refusing to delete — $refuse"
    return 0
  fi

  # Run-window bound: only versions created within this run may go.
  # A dedup'd (identical-rebuild) or otherwise pre-existing version is
  # excluded one by one — the fresh members still go.
  for i in "${!c_ids[@]}"; do
    vc="${c_created[$i]}"
    if [ -z "$vc" ] || [[ "$vc" < "$RUN_STARTED_AT" ]]; then
      echo "$pkg: skipping pre-existing version ${c_ids[$i]} (${c_names[$i]}, created ${vc:-unknown})"
      continue
    fi
    if [ "${c_names[$i]}" = "$idx" ]; then del_idx+=("$i"); else del_now+=("$i"); fi
  done

  # children/referrers first, the index version last.
  for i in ${del_now[@]+"${del_now[@]}"} ${del_idx[@]+"${del_idx[@]}"}; do
    [ -n "$i" ] || continue
    if gh api -X DELETE "orgs/$ORG/packages/container/$pkg/versions/${c_ids[$i]}" >/dev/null; then
      echo "deleted $pkg version ${c_ids[$i]} (${c_names[$i]})"
    else
      echo "::error::delete failed for $pkg version ${c_ids[$i]}"
      rc=1
    fi
  done

  for d in "${!cand[@]}"; do
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
