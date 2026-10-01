#!/usr/bin/env bash
# setup-repo-protection.sh — apply the repo-level release hardening
# (SUPR-5). Everything here 403s on a private repo on the Free plan, so
# run this AFTER the repo (and its GHCR packages) are public:
#
#   1. `release` environment — required reviewers + a deployment tag
#      policy matching `v*` (the publish job runs under environment:
#      release, and GitHub would otherwise auto-create it unprotected).
#      The environment is created BEFORE its tag policy is attached
#      (SUPF-3).
#   2. Tag ruleset for `v*` — creation/update/deletion restricted to repo
#      admins ONLY (SUPF-2: no GitHub Actions app bypass). Consequence:
#      release.yml workflow_dispatch is dry-run only — a real release is
#      made by an admin pushing the tag.
#   3. Branch protection on `main` — required status checks = every ci.yml
#      job EXCEPT the network-dependent chromium freshness job
#      (SUPF-4: it polls Debian security feeds and must not gate merges),
#      enforce admins, dismiss stale reviews. Single maintainer:
#      required_approving_review_count is 0 — raise it (and drop the
#      admin-only bus factor) once a second maintainer exists.
#   4. Private vulnerability reporting + secret scanning with push
#      protection (PUB-10).
#   5. Actions variables ATTESTATIONS_ENABLED and CODE_SCANNING_ENABLED
#      set to `true` (both features need a public repo / GHAS).
#
# Usage:
#   .github/scripts/setup-repo-protection.sh            # dry-run (default)
#   .github/scripts/setup-repo-protection.sh --apply    # make the changes
#
# Auth: `gh auth login` with a token that has admin on the repo.
# Env:
#   GH_REPO            — owner/repo (default: inferred via gh repo view)
#   RELEASE_REVIEWERS  — comma-separated GitHub logins added as required
#                        reviewers of `release` (default: `vanlongme`,
#                        the maintainer; ids are resolved via
#                        `gh api users/<login>` at run time). Repeatable
#                        in a rerun: the environment is PUT with exactly
#                        this set.
#
# Idempotent: live state is GET first; creates/updates happen only where
# the live object is missing or differs. Safe to re-run after config drifts.
# Dry-run prints every planned call as `WOULD: gh api -X <METHOD> <path>`
# followed by the JSON body, so the policy is reviewable before applying.
set -euo pipefail

APPLY=false
[ "${1:-}" = "--apply" ] && APPLY=true

REPO="${GH_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)}"
[ -n "$REPO" ] || { echo "error: cannot determine repo — set GH_REPO=owner/name"; exit 1; }
echo "== repo: $REPO  (mode: $($APPLY && echo APPLY || echo DRY-RUN))"

repo_json="$(gh api "repos/$REPO")"
VIS="$(jq -r .visibility <<< "$repo_json")"
if [ "$VIS" != "public" ]; then
  echo "error: repo is $VIS — environments/rulesets/branch protection need"
  echo "       a public repo on this plan. Make it public first, then re-run."
  exit 1
fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
changed=0
note() { echo "  - $*"; }

# get <endpoint> — prints the body; rc 0 ok, rc 1 on HTTP 404, rc 2 on any
# other API error (callers must fail closed — check the rc).
get() {
  local out
  if out="$(gh api "$1" 2>"$TMP/err")"; then
    printf '%s' "$out"; return 0
  fi
  grep -q 'HTTP 404' "$TMP/err" && return 1
  cat "$TMP/err" >&2
  echo "error: GET $1 failed — refusing to continue" >&2
  return 2
}

# doit <desc> -- <gh api args...> — renders the call, then (in --apply)
# executes it.
doit() {
  local desc="$1"; shift; [ "$1" = "--" ] && shift
  local verb=WOULD; $APPLY && verb=RUN
  echo "  $verb: gh api $*    # $desc"
  if $APPLY; then
    gh api "$@" >/dev/null && note "APPLIED: $desc" || { note "FAILED: $desc"; exit 1; }
  fi
  changed=1
}

# doit_body <desc> <method> <endpoint> <body-file>
doit_body() {
  local desc="$1" method="$2" ep="$3" body="$4"
  local verb=WOULD; $APPLY && verb=RUN
  echo "  $verb: gh api -X $method $ep --input -    # $desc"
  sed 's/^/    /' "$body"
  if $APPLY; then
    gh api -X "$method" "$ep" --input "$body" >/dev/null \
      && note "APPLIED: $desc" || { note "FAILED: $desc"; exit 1; }
  fi
  changed=1
}

# --- 1. release environment ------------------------------------------------
echo "== environment: release"
IFS=',' read -ra _revs <<< "${RELEASE_REVIEWERS:-vanlongme}"
reviewers_json="[]"
for login in "${_revs[@]}"; do
  login="$(echo "$login" | tr -d '[:space:]')"; [ -n "$login" ] || continue
  uid="$(gh api "users/$login" --jq .id)" || { echo "error: no such user '$login'"; exit 1; }
  reviewers_json="$(jq --argjson id "$uid" '. + [{type:"User",id:$id}]' <<< "$reviewers_json")"
done
[ "$reviewers_json" != "[]" ] || { echo "error: RELEASE_REVIEWERS resolved empty"; exit 1; }
note "reviewers: ${_revs[*]}"

jq -n --argjson r "$reviewers_json" '{
  reviewers: $r,
  prevent_self_review: false,
  deployment_branch_policy: {protected_branches: false, custom_branch_policies: true}
}' > "$TMP/env.json"

# SUPF-3: the environment must exist before its deployment tag policy is
# attached. GET returns reviewers nested under protection_rules[].reviewers
# (NOT a top-level .reviewers array).
rc=0; cur="$(get "repos/$REPO/environments/release")" || rc=$?
[ "$rc" -eq 2 ] && exit 1
if [ "$rc" -eq 0 ]; then
  same="$(jq -n --argjson cur "$cur" --argjson want "$reviewers_json" '
    ([$cur.protection_rules[]? | select(.type == "required_reviewers")
      | .reviewers[]? | .reviewer.id] | sort)
    == ([$want[]?.id] | sort)')"
  if [ "$same" = "true" ]; then
    note "ok: reviewers already match"
  else
    doit_body "update reviewers on environment release" PUT \
      "repos/$REPO/environments/release" "$TMP/env.json"
  fi
else
  doit_body "create environment release (reviewers + custom tag policies)" PUT \
    "repos/$REPO/environments/release" "$TMP/env.json"
fi

# SUPF-3: GET deployment-branch-policies returns an OBJECT with a
# branch_policies array ({total_count, branch_policies:[{name,type}]}),
# not a bare array — probe the tag entries by name.
rc=0
tag_policies="$(get "repos/$REPO/environments/release/deployment-branch-policies" \
  | jq -r '.branch_policies[]? | select(.type == "tag") | .name')" || rc=$?
if [ "$rc" -eq 0 ] && printf '%s\n' "$tag_policies" | grep -qxF 'v*'; then
  note "ok: 'v*' tag deployment policy present"
elif [ "$rc" -gt 1 ]; then
  exit 1
else
  doit "add 'v*' tag deployment policy to environment release" -- \
    -X POST "repos/$REPO/environments/release/deployment-branch-policies" \
    -f name='v*' -f type=tag
fi

# --- 2. tag ruleset for v* --------------------------------------------------
# SUPF-2: NO bypass for the GitHub Actions app — tags are pushed by
# admins only; release.yml dispatches are dry-run.
echo "== ruleset: release-tags (v* creation/update/deletion restricted)"
cat > "$TMP/ruleset.json" <<'EOF'
{
  "name": "release-tags",
  "target": "tag",
  "enforcement": "active",
  "bypass_actors": [
    {"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}
  ],
  "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
  "rules": [{"type": "creation"}, {"type": "update"}, {"type": "deletion"}]
}
EOF
rc=0
rs_id="$(get "repos/$REPO/rulesets" \
  | jq -r '.[] | select(.name == "release-tags") | .id' | head -1)" || rc=$?
[ "$rc" -gt 1 ] && exit 1
if [ -n "$rs_id" ]; then
  rc=0; live_rs="$(get "repos/$REPO/rulesets/$rs_id")" || rc=$?
  [ "$rc" -ne 0 ] && exit 1
  # Compare only the fields we manage; the API adds ids, links and timestamps.
  rs_norm='{name, target, enforcement,
    bypass_actors: [.bypass_actors[]? | {actor_id, actor_type, bypass_mode}],
    conditions: {ref_name: {include: (.conditions.ref_name.include // [] | sort),
                            exclude: (.conditions.ref_name.exclude // [] | sort)}},
    rules: [.rules[]? | {type}] | sort_by(.type)}'
  if [ "$(jq -S "$rs_norm" <<< "$live_rs")" = "$(jq -S "$rs_norm" "$TMP/ruleset.json")" ]; then
    note "ok: ruleset release-tags matches the canonical body"
  else
    doit_body "sync tag ruleset release-tags (id=$rs_id) to canonical body" PUT \
      "repos/$REPO/rulesets/$rs_id" "$TMP/ruleset.json"
  fi
else
  doit_body "create tag ruleset release-tags" POST \
    "repos/$REPO/rulesets" "$TMP/ruleset.json"
fi

# --- 3. branch protection on main -------------------------------------------
# SUPF-4: required_approving_review_count 0 while the repo has a single
# maintainer; the chromium freshness job stays OUT of the required
# contexts — it polls the Debian security feed (network-dependent).
echo "== branch protection: main"
cat > "$TMP/protection.json" <<'EOF'
{
  "required_status_checks": {
    "strict": true,
    "contexts": [
      "go vet + test -race (envtest)",
      "integration (postgres service + envtest)",
      "portal ui (npm ci, tsc, vitest, build)",
      "chart (helm lint --strict + chart tests)",
      "govulncheck (Go vuln scan)",
      "dependency review (PRs)",
      "workflow policy tests (.github)",
      "workflow lint (actionlint + yamllint + zizmor)"
    ]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "dismiss_stale_reviews": true,
    "required_approving_review_count": 0
  },
  "restrictions": null,
  "required_linear_history": false,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "block_creations": false,
  "required_conversation_resolution": true,
  "lock_branch": false,
  "allow_fork_syncing": false
}
EOF
rc=0; live_bp="$(get "repos/$REPO/branches/main/protection")" || rc=$?
[ "$rc" -eq 2 ] && exit 1
if [ "$rc" -eq 0 ]; then
  # GET returns {enabled: bool} wrappers and extra keys; map it onto the
  # PUT body's shape before comparing.
  bp_live='{
    required_status_checks: {strict: (.required_status_checks.strict // false),
                             contexts: (.required_status_checks.contexts // [] | sort)},
    enforce_admins: (.enforce_admins.enabled // false),
    required_pull_request_reviews: (if .required_pull_request_reviews then
      {dismiss_stale_reviews: (.required_pull_request_reviews.dismiss_stale_reviews // false),
       required_approving_review_count: (.required_pull_request_reviews.required_approving_review_count // 0)}
      else null end),
    restrictions: (.restrictions // null),
    required_linear_history: (.required_linear_history.enabled // false),
    allow_force_pushes: (.allow_force_pushes.enabled // false),
    allow_deletions: (.allow_deletions.enabled // false),
    block_creations: (.block_creations.enabled // false),
    required_conversation_resolution: (.required_conversation_resolution.enabled // false),
    lock_branch: (.lock_branch.enabled // false),
    allow_fork_syncing: (.allow_fork_syncing.enabled // false)}'
  if [ "$(jq -S "$bp_live" <<< "$live_bp")" = "$(jq -S '.required_status_checks.contexts |= sort' "$TMP/protection.json")" ]; then
    note "ok: main branch protection matches the canonical body"
  else
    doit_body "update main branch protection to canonical body" PUT \
      "repos/$REPO/branches/main/protection" "$TMP/protection.json"
  fi
else
  doit_body "create main branch protection (required checks = all ci jobs)" PUT \
    "repos/$REPO/branches/main/protection" "$TMP/protection.json"
fi

# --- 4. security features (PUB-10) -------------------------------------------
echo "== security features"
rc=0; pvr="$(get "repos/$REPO/private-vulnerability-reporting")" || rc=$?
[ "$rc" -eq 2 ] && exit 1
if [ "$rc" -eq 0 ] && [ "$(jq -r '.enabled // false' <<< "$pvr")" = "true" ]; then
  note "ok: private vulnerability reporting already enabled"
else
  doit "enable private vulnerability reporting" -- \
    -X PUT "repos/$REPO/private-vulnerability-reporting"
fi
# Secret scanning + push protection via security_and_analysis.
sa="$(jq -c '.security_and_analysis // {}' <<< "$repo_json")"
if [ "$(jq -r '.secret_scanning.status // "disabled"' <<< "$sa")" = "enabled" ] \
  && [ "$(jq -r '.secret_scanning_push_protection.status // "disabled"' <<< "$sa")" = "enabled" ]; then
  note "ok: secret scanning + push protection already enabled"
else
  cat > "$TMP/secfeatures.json" <<'EOF'
{
  "security_and_analysis": {
    "secret_scanning": {"status": "enabled"},
    "secret_scanning_push_protection": {"status": "enabled"}
  }
}
EOF
  doit_body "enable secret scanning + push protection" PATCH \
    "repos/$REPO" "$TMP/secfeatures.json"
fi

# --- 5. Actions variables ---------------------------------------------------
echo "== Actions variables"
for kv in "ATTESTATIONS_ENABLED=true" "CODE_SCANNING_ENABLED=true"; do
  name="${kv%%=*}"; want="${kv#*=}"
  rc=0; cur="$(get "repos/$REPO/actions/variables/$name")" || rc=$?
  [ "$rc" -eq 2 ] && exit 1
  if [ "$rc" -eq 0 ]; then
    if [ "$(jq -r .value <<< "$cur")" = "$want" ]; then
      note "ok: $name already $want"
    else
      doit "update variable $name -> $want" -- \
        -X PATCH "repos/$REPO/actions/variables/$name" -f value="$want"
    fi
  else
    doit "create variable $name=$want" -- \
      -X POST "repos/$REPO/actions/variables" -f name="$name" -f value="$want"
  fi
done

echo
if $APPLY; then
  echo "done ($changed change(s) applied)."
else
  echo "dry-run complete — re-run with --apply to make $changed pending change(s)."
fi
