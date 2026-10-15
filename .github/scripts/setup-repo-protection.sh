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
#   2. `runtime-train` environment — required reviewers (TRAIN_REVIEWERS,
#      default: the RELEASE_REVIEWERS set) + a deployment branch policy
#      restricted to `main`. The train's publish job runs under
#      environment: runtime-train (SR-5-F2 mitigation — every train
#      publish waits on a recorded approval). Same SUPF-3 ordering: the
#      environment is created BEFORE its branch policy is attached.
#   3. Tag ruleset for `v*` — creation/update/deletion restricted to repo
#      admins ONLY (SUPF-2: no GitHub Actions app bypass). Consequence:
#      release.yml workflow_dispatch is dry-run only — a real release is
#      made by an admin pushing the tag.
#   4. Branch protection on `main` — required status checks = every ci.yml
#      job EXCEPT the network-dependent ones (SUPF-4), which pull
#      multi-GB images or boot a kind cluster, so an outage upstream
#      would hold every merge hostage:
#        - "kasm adapter contract + catalog scan" (pulls the Docker Hub
#          kasmweb catalog images; main pushes, the weekly schedule and
#          the kasm path gate on PRs still run it, and the declared
#          catalog floor is checked by the required workflow-policy job);
#        - "quickstart (kind up.sh + portal smoke)";
#        - "partitioned (kind up.sh + CHIPS cookie smoke)".
#      .github/tests/setup-repo-protection.test.sh derives the ci.yml job
#      names and fails when a job is neither required nor listed here.
#      Enforce admins,
#      dismiss stale reviews. Single maintainer today, so
#      required_approving_review_count is 0 — when a second maintainer
#      joins: set
#      required_approving_review_count 1 + require_last_push_approval on
#      main, and prevent_self_review true on the `release` and
#      `runtime-train` environments.
#   5. Private vulnerability reporting + secret scanning with push
#      protection (PUB-10).
#   6. Actions variables ATTESTATIONS_ENABLED and CODE_SCANNING_ENABLED
#      set to `true` (both features need a public repo / GHAS).
#   7. Repository Actions permissions — sha_pinning_required=true
#      (SR-5-F5): the platform then refuses any `uses:` not pinned to a
#      full-length commit SHA. Applied as a PUT that preserves the live
#      `enabled` / `allowed_actions` values.
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
#   TRAIN_REVIEWERS    — same for `runtime-train` (default: the
#                        RELEASE_REVIEWERS set).
#
# Idempotent: live state is GET first; creates/updates happen only where
# the live object is missing or differs. The environment compare covers
# every managed field (reviewers, wait_timer, prevent_self_review and the
# deployment_branch_policy mode); the deployment branch/tag policy set is
# managed exactly — a missing entry is POSTed and an unexpected entry is
# planned for DELETE by id, capped at 3 per environment (more aborts the
# run for manual review). Safe to re-run after config drifts.
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
# Safety bound on the delete-extras repair: more than this many unexpected
# deployment policies in one environment aborts the run for manual review
# instead of planning a mass deletion.
MAX_POLICY_DELETES=3
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
  changed=$((changed + 1))
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
  changed=$((changed + 1))
}

# env_matches <live-env-json> <wanted-body-file> — "true" only when every
# field the env PUT manages is already what we would write: reviewer ids,
# prevent_self_review (a field of the required_reviewers protection rule),
# wait_timer (its protection rule is absent when the timer is 0) and
# deployment_branch_policy (the custom branch/tag policy mode). GET adds
# ids/links/timestamps, so only the managed fields are compared — drift in
# any of them plans a re-PUT that repairs all of them at once.
env_matches() {
  jq -n --argjson cur "$1" --argjson want "$(cat "$2")" '
    ([$cur.protection_rules[]? | select(.type == "required_reviewers")
        | .reviewers[]? | .reviewer.id] | sort)
      == ([$want.reviewers[]?.id] | sort)
    and (([$cur.protection_rules[]? | select(.type == "required_reviewers")
        | .prevent_self_review] | .[0]) // false)
      == ($want.prevent_self_review // false)
    and (([$cur.protection_rules[]? | select(.type == "wait_timer")
        | .wait_timer] | .[0]) // 0)
      == ($want.wait_timer // 0)
    and ($cur.deployment_branch_policy // null)
      == ($want.deployment_branch_policy // null)'
}

# pol_drift <policies-json> <wanted-name> <wanted-type> — the managed
# deployment branch/tag policy set is exactly one entry; prints one
# tab-separated drift line per difference:
#   "missing"                          wanted entry absent -> POST it
#   "extra <id> <name> <type>"         unexpected entry -> DELETE it by id
# Fails closed on a malformed-success: an empty/non-JSON body or a partial
# page (total_count above the entries the ?per_page=100 GET returned) is an
# error, never an "all clear".
pol_drift() {
  jq -e 'type == "object" and (.branch_policies | type) == "array"
         and (.total_count | type) == "number"' <<< "$1" >/dev/null || {
    echo "error: deployment-branch-policies GET returned a malformed body — refusing to continue" >&2
    return 2
  }
  if [ "$(jq -r '.total_count > (.branch_policies | length)' <<< "$1")" = "true" ]; then
    echo "error: deployment-branch-policies GET returned a partial page — total_count" >&2
    echo "       exceeds the entries fetched (?per_page=100). Inspect the environment" >&2
    echo "       manually, then re-run." >&2
    return 2
  fi
  jq -r --arg n "$2" --arg t "$3" '
    (.branch_policies // []) as $p
    | (if [$p[] | select(.name == $n and .type == $t)] == []
       then ["missing"] else [] end)
      + [$p[] | select(.name != $n or .type != $t)
             | "extra\t\(.id)\t\(.name)\t\(.type)"]
    | .[]' <<< "$1"
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
  wait_timer: 0,
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
  if [ "$(env_matches "$cur" "$TMP/env.json")" = "true" ]; then
    note "ok: environment release settings already match"
  else
    doit_body "sync environment release settings" PUT \
      "repos/$REPO/environments/release" "$TMP/env.json"
  fi
else
  doit_body "create environment release (reviewers + custom tag policies)" PUT \
    "repos/$REPO/environments/release" "$TMP/env.json"
fi

# SUPF-3: GET deployment-branch-policies returns an OBJECT with a
# branch_policies array ({total_count, branch_policies:[{id,name,type}]}),
# not a bare array. The managed set is exactly {'v*' tag} — drift in either
# direction (entry missing or an unexpected extra) plans a repair.
rc=0
tag_policies="$(get "repos/$REPO/environments/release/deployment-branch-policies?per_page=100")" || rc=$?
if [ "$rc" -eq 0 ]; then
  drift="$(pol_drift "$tag_policies" 'v*' 'tag')"
  if [ -z "$drift" ]; then
    note "ok: 'v*' tag deployment policy present"
  else
    if [ "$(printf '%s\n' "$drift" | grep -c '^extra' || true)" -gt "$MAX_POLICY_DELETES" ]; then
      echo "error: environment release has more than $MAX_POLICY_DELETES unexpected" >&2
      echo "       deployment policies — refusing to plan a mass deletion. Inspect and" >&2
      echo "       remove them manually (gh api repos/$REPO/environments/release/" >&2
      echo "       deployment-branch-policies), then re-run." >&2
      exit 1
    fi
    while IFS=$'\t' read -r kind pid pname ptype; do
      if [ "$kind" = missing ]; then
        doit "add 'v*' tag deployment policy to environment release" -- \
          -X POST "repos/$REPO/environments/release/deployment-branch-policies" \
          -f name='v*' -f type=tag
      else
        doit "remove unexpected '$pname' $ptype deployment policy on environment release" -- \
          -X DELETE "repos/$REPO/environments/release/deployment-branch-policies/$pid"
      fi
    done <<< "$drift"
  fi
elif [ "$rc" -gt 1 ]; then
  exit 1
else
  doit "add 'v*' tag deployment policy to environment release" -- \
    -X POST "repos/$REPO/environments/release/deployment-branch-policies" \
    -f name='v*' -f type=tag
fi

# --- 2. runtime-train environment --------------------------------------------
# SR-5-F2 mitigation: the train's publish job runs under this
# environment, so every rt-* publish waits on a recorded approval.
# Only `main` may deploy to it.
echo "== environment: runtime-train"
IFS=',' read -ra _trevs <<< "${TRAIN_REVIEWERS:-${RELEASE_REVIEWERS:-vanlongme}}"
train_reviewers_json="[]"
for login in "${_trevs[@]}"; do
  login="$(echo "$login" | tr -d '[:space:]')"; [ -n "$login" ] || continue
  uid="$(gh api "users/$login" --jq .id)" || { echo "error: no such user '$login'"; exit 1; }
  train_reviewers_json="$(jq --argjson id "$uid" '. + [{type:"User",id:$id}]' <<< "$train_reviewers_json")"
done
[ "$train_reviewers_json" != "[]" ] \
  || { echo "error: TRAIN_REVIEWERS resolved empty"; exit 1; }
note "runtime-train reviewers: ${_trevs[*]}"

jq -n --argjson r "$train_reviewers_json" '{
  wait_timer: 0,
  reviewers: $r,
  prevent_self_review: false,
  deployment_branch_policy: {protected_branches: false, custom_branch_policies: true}
}' > "$TMP/env-train.json"

rc=0; cur="$(get "repos/$REPO/environments/runtime-train")" || rc=$?
[ "$rc" -eq 2 ] && exit 1
if [ "$rc" -eq 0 ]; then
  if [ "$(env_matches "$cur" "$TMP/env-train.json")" = "true" ]; then
    note "ok: environment runtime-train settings already match"
  else
    doit_body "sync environment runtime-train settings" PUT \
      "repos/$REPO/environments/runtime-train" "$TMP/env-train.json"
  fi
else
  doit_body "create environment runtime-train (reviewers + custom branch policies)" PUT \
    "repos/$REPO/environments/runtime-train" "$TMP/env-train.json"
fi

# SUPF-3 ordering, same as `release`: the environment must exist before
# its deployment branch policy is attached, and the managed set is exactly
# {'main' branch} — drift in either direction plans a repair.
rc=0
train_branch_policies="$(get "repos/$REPO/environments/runtime-train/deployment-branch-policies?per_page=100")" || rc=$?
if [ "$rc" -eq 0 ]; then
  drift="$(pol_drift "$train_branch_policies" 'main' 'branch')"
  if [ -z "$drift" ]; then
    note "ok: 'main' branch deployment policy present"
  else
    if [ "$(printf '%s\n' "$drift" | grep -c '^extra' || true)" -gt "$MAX_POLICY_DELETES" ]; then
      echo "error: environment runtime-train has more than $MAX_POLICY_DELETES unexpected" >&2
      echo "       deployment policies — refusing to plan a mass deletion. Inspect and" >&2
      echo "       remove them manually (gh api repos/$REPO/environments/runtime-train/" >&2
      echo "       deployment-branch-policies), then re-run." >&2
      exit 1
    fi
    while IFS=$'\t' read -r kind pid pname ptype; do
      if [ "$kind" = missing ]; then
        doit "add 'main' branch deployment policy to environment runtime-train" -- \
          -X POST "repos/$REPO/environments/runtime-train/deployment-branch-policies" \
          -f name='main' -f type=branch
      else
        doit "remove unexpected '$pname' $ptype deployment policy on environment runtime-train" -- \
          -X DELETE "repos/$REPO/environments/runtime-train/deployment-branch-policies/$pid"
      fi
    done <<< "$drift"
  fi
elif [ "$rc" -gt 1 ]; then
  exit 1
else
  doit "add 'main' branch deployment policy to environment runtime-train" -- \
    -X POST "repos/$REPO/environments/runtime-train/deployment-branch-policies" \
    -f name='main' -f type=branch
fi

# --- 3. tag ruleset for v* --------------------------------------------------
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

# --- 4. branch protection on main -------------------------------------------
# SUPF-4: required_approving_review_count 0 while the repo has a single
# maintainer; the contexts are every ci.yml job except the network-dependent
# ones listed in the header (item 4) — R1-39.
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
      "workflow lint (actionlint + yamllint + zizmor)",
      "soak harness (npm ci, tsc, unit + dry-run)",
      "go fuzz (auth/session parsing boundaries)"
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

# --- 5. security features (PUB-10) -------------------------------------------
echo "== security features"
rc=0; pvr="$(get "repos/$REPO/private-vulnerability-reporting")" || rc=$?
[ "$rc" -eq 2 ] && exit 1
if [ "$rc" -eq 0 ] && [ "$(jq -r '.enabled // false' <<< "$pvr")" = "true" ]; then
  note "ok: private vulnerability reporting already enabled"
else
  doit "enable private vulnerability reporting" -- \
    -X PUT "repos/$REPO/private-vulnerability-reporting"
fi
# Dependabot alerts (turns on the dependency graph that ci.yml's
# dependency-review job needs). GET answers 204 when on, 404 when off.
if gh api "repos/$REPO/vulnerability-alerts" >/dev/null 2>&1; then
  note "ok: Dependabot alerts / dependency graph already enabled"
else
  doit "enable Dependabot alerts (dependency graph)" -- \
    -X PUT "repos/$REPO/vulnerability-alerts"
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

# --- 6. Actions variables ---------------------------------------------------
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

# --- 7. Actions permissions: SHA pinning (SR-5-F5) ---------------------------
# sha_pinning_required makes the platform enforce what the workflows
# already do by convention — every `uses:` pinned to a full commit SHA.
# The PUT must carry the live `enabled` / `allowed_actions` values, so a
# GET that returns nothing usable fails closed rather than guessing
# settings onto the endpoint.
echo "== actions permissions (sha_pinning_required)"
rc=0; ap="$(get "repos/$REPO/actions/permissions")" || rc=$?
if [ "$rc" -ne 0 ]; then
  echo "error: cannot read actions/permissions — refusing to continue" >&2
  exit 1
fi
if [ "$(jq -r '.sha_pinning_required // false' <<< "$ap")" = "true" ]; then
  note "ok: sha_pinning_required already true"
else
  jq -n --argjson cur "$ap" '{
    enabled: ($cur.enabled // false),
    allowed_actions: ($cur.allowed_actions // "all"),
    sha_pinning_required: true
  }' > "$TMP/actions-perms.json"
  doit_body "enforce sha_pinning_required (preserving enabled/allowed_actions)" PUT \
    "repos/$REPO/actions/permissions" "$TMP/actions-perms.json"
fi

echo
if $APPLY; then
  echo "done ($changed change(s) applied)."
else
  echo "dry-run complete — re-run with --apply to make $changed pending change(s)."
fi
