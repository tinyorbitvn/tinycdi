#!/usr/bin/env bash
# setup-repo-protection.test.sh — render the protection script's dry-run
# plan against a stubbed `gh` and assert the policy it would apply
# (SUPF-2/3/4):
#   * v* tag ruleset bypasses repo admins ONLY (no GitHub Actions app)
#   * environment is created BEFORE its deployment tag policy
#   * reviewers compared via protection_rules[].reviewers (SUPF-3 field)
#   * required_approving_review_count: 0 (single maintainer), and the
#     network-dependent "chromium apt-pin freshness" is NOT required
#   * private vulnerability reporting + secret scanning/push protection
#   * runtime-train environment (SR-5-F2): reviewers + wait_timer 0 +
#     prevent_self_review false + a `main` branch deployment policy,
#     created after the environment exists (SUPF-3 ordering)
#   * environment compare covers reviewers + wait_timer +
#     prevent_self_review + deployment_branch_policy, and the deployment
#     branch/tag policy set must match exactly (extra entries are removed)
#   * the summary counter equals the number of planned calls, not sections
#   * sha_pinning_required=true on repos/…/actions/permissions (SR-5-F5),
#     PUT preserving the live enabled/allowed_actions values
#   * GET probes fail closed on non-404 API errors (SUPF-9)
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/.github/scripts/setup-repo-protection.sh"
fails=0

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# --- gh stub: canned GETs; --jq applied through real jq -------------------
cat > "$WORK/bin/gh" <<'EOF'
#!/usr/bin/env bash
# gh stub — state selected via GH_STUB_MODE: clean | partial | converged | err500
# Single-field drift on the converged state via GH_STUB_DRIFT:
#   rel-psr | rel-wait | rel-dbp             (release environment)
#   train-psr | train-wait | train-dbp       (runtime-train)
#   rel-pol | train-pol                      extra deployment policy
#   rel-pol-miss | train-pol-miss            GET 200, wanted policy absent
#   rel-pol-pages | train-pol-pages          partial page (total_count > entries)
#   rel-pol-many | train-pol-many            extras over the DELETE bound
#   rel-pol-empty | train-pol-empty          empty policies body
#   rel-pol-bad | train-pol-bad              non-JSON policies body
[ "$1" = "api" ] || { echo "gh stub: unsupported '$*'" >&2; exit 2; }
shift
endpoint="" ; jqexpr=""
while [ $# -gt 0 ]; do
  case "$1" in
    --jq) jqexpr="$2"; shift 2 ;;
    --paginate) shift ;;
    -X|-f|-F|--input) shift 2 ;;
    -*) shift ;;
    *) [ -z "$endpoint" ] && endpoint="$1"; shift ;;
  esac
done
# endpoints may carry a query string (the policies GETs must pass
# ?per_page=100) — epath is the route, equery the raw query.
epath="$endpoint"; equery=""
case "$endpoint" in *\?*) epath="${endpoint%%\?*}"; equery="${endpoint#*\?}" ;; esac
reply() { # reply <body>
  if [ -n "$jqexpr" ]; then printf '%s' "$1" | jq -r "$jqexpr"; else printf '%s\n' "$1"; fi
  exit 0
}
notfound() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
err500()   { echo "gh: boom (HTTP 500)" >&2; exit 1; }
mode="${GH_STUB_MODE:-clean}"
drift="${GH_STUB_DRIFT:-}"
# err500<where> modes replay a fully converged repo with one failing GET.
fail_ep=""
case "$mode" in
  err500train) mode=converged; fail_ep="repos/test/repo/environments/runtime-train" ;;
  err500perms) mode=converged; fail_ep="repos/test/repo/actions/permissions" ;;
esac
[ -n "$fail_ep" ] && [ "$epath" = "$fail_ep" ] && err500
case "$epath" in
  repos/test/repo)
    [ "$mode" = "converged" ] && reply '{"visibility":"public","security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}'
    reply '{"visibility":"public","security_and_analysis":{}}' ;;
  user)                     reply '{"login":"octocat"}' ;;
  users/vanlongme)          reply '{"id":1234}' ;;
  repos/test/repo/environments/release)
    [ "$mode" = "err500" ] && err500
    case "$mode" in partial|converged)
      case "$drift" in
        rel-psr)  reply '{"protection_rules":[{"type":"required_reviewers","prevent_self_review":true,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' ;;
        rel-wait) reply '{"protection_rules":[{"type":"wait_timer","wait_timer":30},{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' ;;
        rel-dbp)  reply '{"protection_rules":[{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false}}' ;;
      esac
      reply '{"protection_rules":[{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' ;;
    esac
    notfound ;;
  repos/test/repo/environments/release/deployment-branch-policies)
    [ "$equery" = "per_page=100" ] || { echo "gh stub: policies GET lacks ?per_page=100" >&2; exit 2; }
    case "$mode" in partial|converged)
      case "$drift" in
        rel-pol)       reply '{"total_count":2,"branch_policies":[{"id":11,"name":"v*","type":"tag"},{"id":12,"name":"dev*","type":"tag"}]}' ;;
        rel-pol-miss)  reply '{"total_count":0,"branch_policies":[]}' ;;
        rel-pol-pages) reply '{"total_count":200,"branch_policies":[{"id":11,"name":"v*","type":"tag"},{"id":12,"name":"dev*","type":"tag"}]}' ;;
        rel-pol-many)  reply '{"total_count":5,"branch_policies":[{"id":11,"name":"v*","type":"tag"},{"id":12,"name":"a*","type":"tag"},{"id":13,"name":"b*","type":"tag"},{"id":14,"name":"c*","type":"tag"},{"id":15,"name":"d*","type":"tag"}]}' ;;
        rel-pol-empty) reply '' ;;
        rel-pol-bad)   reply 'not-json' ;;
      esac
      reply '{"total_count":1,"branch_policies":[{"id":11,"name":"v*","type":"tag"}]}' ;;
    esac
    notfound ;;
  repos/test/repo/environments/runtime-train)
    case "$mode" in partial|converged)
      case "$drift" in
        train-psr)  reply '{"protection_rules":[{"type":"required_reviewers","prevent_self_review":true,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' ;;
        train-wait) reply '{"protection_rules":[{"type":"wait_timer","wait_timer":30},{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' ;;
        train-dbp)  reply '{"protection_rules":[{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false}}' ;;
      esac
      reply '{"protection_rules":[{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]},{"type":"branch_policy"}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' ;;
    esac
    notfound ;;
  repos/test/repo/environments/runtime-train/deployment-branch-policies)
    [ "$equery" = "per_page=100" ] || { echo "gh stub: policies GET lacks ?per_page=100" >&2; exit 2; }
    if [ "$mode" = "converged" ]; then
      case "$drift" in
        train-pol)       reply '{"total_count":2,"branch_policies":[{"id":21,"name":"main","type":"branch"},{"id":22,"name":"dev","type":"branch"}]}' ;;
        train-pol-miss)  reply '{"total_count":0,"branch_policies":[]}' ;;
        train-pol-pages) reply '{"total_count":200,"branch_policies":[{"id":21,"name":"main","type":"branch"}]}' ;;
        train-pol-many)  reply '{"total_count":5,"branch_policies":[{"id":21,"name":"main","type":"branch"},{"id":22,"name":"a","type":"branch"},{"id":23,"name":"b","type":"branch"},{"id":24,"name":"c","type":"branch"},{"id":25,"name":"d","type":"branch"}]}' ;;
        train-pol-empty) reply '' ;;
        train-pol-bad)   reply 'not-json' ;;
      esac
      reply '{"total_count":1,"branch_policies":[{"id":21,"name":"main","type":"branch"}]}'
    fi
    notfound ;;
  repos/test/repo/rulesets)
    case "$mode" in partial|converged) reply '[{"id":7,"name":"release-tags"}]' ;; esac
    reply '[]' ;;
  repos/test/repo/rulesets/7)
    [ "$mode" = "converged" ] && reply '{"id":7,"name":"release-tags","target":"tag","source_type":"Repository","enforcement":"active","bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}],"conditions":{"ref_name":{"exclude":[],"include":["refs/tags/v*"]}},"rules":[{"type":"creation"},{"type":"update"},{"type":"deletion"}],"created_at":"2026-01-01T00:00:00Z"}'
    reply '{"id":7,"name":"release-tags","target":"tag","enforcement":"evaluate","bypass_actors":[],"conditions":{"ref_name":{"exclude":[],"include":["refs/tags/v*"]}},"rules":[{"type":"deletion"}]}' ;;
  repos/test/repo/branches/main/protection)
    [ "$mode" = "partial" ] && reply '{}'
    [ "$mode" = "converged" ] && reply '{"url":"x","required_status_checks":{"strict":true,"contexts":["workflow lint (actionlint + yamllint + zizmor)","go vet + test -race (envtest)","integration (postgres service + envtest)","portal ui (npm ci, tsc, vitest, build)","chart (helm lint --strict + chart tests)","govulncheck (Go vuln scan)","dependency review (PRs)","workflow policy tests (.github)","soak harness (npm ci, tsc, unit + dry-run)","go fuzz (auth/session parsing boundaries)"],"checks":[]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"dismiss_stale_reviews":true,"require_code_owner_reviews":false,"required_approving_review_count":0},"required_linear_history":{"enabled":false},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"block_creations":{"enabled":false},"required_conversation_resolution":{"enabled":true},"lock_branch":{"enabled":false},"allow_fork_syncing":{"enabled":false}}'
    notfound ;;
  repos/test/repo/vulnerability-alerts)
    [ "$mode" = "converged" ] && reply ''
    notfound ;;
  repos/test/repo/private-vulnerability-reporting)
    [ "$mode" = "converged" ] && reply '{"enabled":true}'
    reply '{"enabled":false}' ;;
  repos/test/repo/actions/variables/ATTESTATIONS_ENABLED)
    case "$mode" in partial|converged) reply '{"name":"ATTESTATIONS_ENABLED","value":"true"}' ;; esac
    notfound ;;
  repos/test/repo/actions/variables/CODE_SCANNING_ENABLED)
    [ "$mode" = "partial" ] && reply '{"name":"CODE_SCANNING_ENABLED","value":"false"}'
    [ "$mode" = "converged" ] && reply '{"name":"CODE_SCANNING_ENABLED","value":"true"}'
    notfound ;;
  repos/test/repo/actions/permissions)
    [ "$mode" = "converged" ] && reply '{"enabled":true,"allowed_actions":"local_only","sha_pinning_required":true}'
    # non-default allowed_actions proves the PUT preserves live values
    reply '{"enabled":true,"allowed_actions":"local_only","sha_pinning_required":false}' ;;
  *) echo "gh stub: unmocked endpoint '$endpoint'" >&2; exit 2 ;;
esac
EOF
chmod +x "$WORK/bin/gh"

chk()    { grep -qE -e "$2" "$1" || { echo "FAIL: $3"; fails=1; }; }
absent() { grep -qE -e "$2" "$1" && { echo "FAIL: $3"; fails=1; } || true; }

# ---- scenario: clean slate ----------------------------------------------
GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=clean \
  bash "$SCRIPT" > "$WORK/clean.out" 2>&1 || { echo "FAIL: clean dry-run exited nonzero"; cat "$WORK/clean.out"; fails=1; }

chk  "$WORK/clean.out" 'WOULD: gh api -X PUT repos/test/repo/environments/release' "env PUT not planned"
chk  "$WORK/clean.out" '"type": *"User"'    "env body lacks reviewer"
chk  "$WORK/clean.out" '"id": *1234'        "env body lacks reviewer id"
chk  "$WORK/clean.out" 'custom_branch_policies": *true' "env body lacks custom policies"
chk  "$WORK/clean.out" 'WOULD: gh api -X POST repos/test/repo/environments/release/deployment-branch-policies' "tag policy POST not planned"
# SUPF-3 ordering: environment PUT strictly before the policy POST.
env_ln="$(grep -n 'WOULD: gh api -X PUT repos/test/repo/environments/release' "$WORK/clean.out" | head -1 | cut -d: -f1)"
pol_ln="$(grep -n 'WOULD: gh api -X POST repos/test/repo/environments/release/deployment-branch-policies' "$WORK/clean.out" | head -1 | cut -d: -f1)"
[ -n "$env_ln" ] && [ -n "$pol_ln" ] && [ "$env_ln" -lt "$pol_ln" ] \
  || { echo "FAIL: environment not created before its tag policy"; fails=1; }

# SUPF-2: ruleset bypass is admin role only — never the Actions app.
chk    "$WORK/clean.out" 'WOULD: gh api -X POST repos/test/repo/rulesets' "ruleset POST not planned"
chk    "$WORK/clean.out" '"actor_id": *5'   "ruleset lacks admin bypass"
absent "$WORK/clean.out" '15368'            "ruleset still bypasses GitHub Actions app"
absent "$WORK/clean.out" 'Integration'      "ruleset still grants an Integration bypass"

# SUPF-4: single maintainer -> 0 reviews; freshness job not required.
chk    "$WORK/clean.out" 'WOULD: gh api -X PUT repos/test/repo/branches/main/protection' "protection PUT not planned"
chk    "$WORK/clean.out" '"required_approving_review_count": *0' "review count is not 0"
chk    "$WORK/clean.out" 'workflow lint \(actionlint \+ yamllint \+ zizmor\)' "required checks missing lint job"
absent "$WORK/clean.out" 'chromium apt-pin freshness' "network freshness check must not be required"

# PUB-10: security features enabled.
chk "$WORK/clean.out" 'WOULD: gh api -X PUT repos/test/repo/private-vulnerability-reporting' "PVR not planned"
chk "$WORK/clean.out" 'WOULD: gh api -X PUT repos/test/repo/vulnerability-alerts' "Dependabot alerts not planned"
chk "$WORK/clean.out" 'WOULD: gh api -X PATCH repos/test/repo' "security_and_analysis PATCH not planned"
chk "$WORK/clean.out" '"secret_scanning_push_protection"' "push protection not in PATCH body"
chk "$WORK/clean.out" 'WOULD: gh api -X POST repos/test/repo/actions/variables' "variables not planned"
chk "$WORK/clean.out" 'dry-run complete' "dry-run did not finish cleanly"

# The summary counter counts every planned call, not sections: pin the
# exact number for this multi-change stub and cross-check it against the
# WOULD line count.
nw="$(grep -c 'WOULD:' "$WORK/clean.out")"
chk "$WORK/clean.out" "make ${nw} pending change" "summary counter disagrees with the WOULD lines"
chk "$WORK/clean.out" 'make 12 pending change' "clean stub plans other than 12 changes"

# SR-5-F2: the runtime-train environment is planned with reviewers +
# wait_timer 0 + prevent_self_review false + a `main` branch policy,
# environment PUT strictly before the policy POST (SUPF-3 ordering).
# Body assertions are scoped to the runtime-train PUT body itself — the
# release body also prints "prevent_self_review": false.
chk  "$WORK/clean.out" 'WOULD: gh api -X PUT repos/test/repo/environments/runtime-train' "runtime-train env PUT not planned"
awk '/WOULD: gh api -X PUT .*environments\/runtime-train/{f=1;next} f&&/^    /{print;next} f{exit}' \
  "$WORK/clean.out" > "$WORK/train-body.json"
chk  "$WORK/train-body.json" '"wait_timer": *0'            "runtime-train env body lacks wait_timer 0"
chk  "$WORK/train-body.json" '"prevent_self_review": *false' "runtime-train env body lacks prevent_self_review false"
chk  "$WORK/train-body.json" '"custom_branch_policies": *true' "runtime-train env body lacks custom policies"
chk  "$WORK/train-body.json" '"id": *1234'                  "runtime-train env body lacks reviewer id"
chk  "$WORK/clean.out" 'WOULD: gh api -X POST repos/test/repo/environments/runtime-train/deployment-branch-policies -f name=main -f type=branch' "runtime-train 'main' branch policy POST not planned"
trenv_ln="$(grep -n 'WOULD: gh api -X PUT repos/test/repo/environments/runtime-train' "$WORK/clean.out" | head -1 | cut -d: -f1)"
trpol_ln="$(grep -n 'WOULD: gh api -X POST repos/test/repo/environments/runtime-train/deployment-branch-policies' "$WORK/clean.out" | head -1 | cut -d: -f1)"
[ -n "$trenv_ln" ] && [ -n "$trpol_ln" ] && [ "$trenv_ln" -lt "$trpol_ln" ] \
  || { echo "FAIL: runtime-train environment not created before its branch policy"; fails=1; }

# SR-5-F5: sha_pinning_required PUT preserves live enabled/allowed_actions.
chk  "$WORK/clean.out" 'WOULD: gh api -X PUT repos/test/repo/actions/permissions' "sha-pinning PUT not planned"
chk  "$WORK/clean.out" '"sha_pinning_required": *true' "PUT body lacks sha_pinning_required:true"
chk  "$WORK/clean.out" '"enabled": *true'             "PUT body drops live enabled value"
chk  "$WORK/clean.out" '"allowed_actions": *"local_only"' "PUT body does not preserve allowed_actions"

# ---- scenario: converged state -------------------------------------------
GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=partial \
  bash "$SCRIPT" > "$WORK/partial.out" 2>&1 || { echo "FAIL: partial dry-run exited nonzero"; cat "$WORK/partial.out"; fails=1; }
chk "$WORK/partial.out" 'ok: environment release settings already match' "idempotent: release env re-detected wrongly"
chk "$WORK/partial.out" "ok: 'v\*' tag deployment policy present" "idempotent: tag policy re-detected wrongly"
chk "$WORK/partial.out" 'WOULD: gh api -X PUT repos/test/repo/rulesets/7' "idempotent: ruleset sync missing"
chk "$WORK/partial.out" 'ok: ATTESTATIONS_ENABLED already true' "idempotent: variable re-detected wrongly"
chk "$WORK/partial.out" 'WOULD: gh api -X PATCH repos/test/repo/actions/variables/CODE_SCANNING_ENABLED' "idempotent: stale variable not PATCHed"

chk "$WORK/partial.out" 'WOULD: gh api -X PUT repos/test/repo/branches/main/protection' "idempotent: protection drift not detected"
chk "$WORK/partial.out" 'ok: environment runtime-train settings already match' "idempotent: runtime-train env re-detected wrongly"
chk "$WORK/partial.out" 'WOULD: gh api -X POST repos/test/repo/environments/runtime-train/deployment-branch-policies' "idempotent: missing 'main' branch policy not POSTed"

np="$(grep -c 'WOULD:' "$WORK/partial.out")"
chk "$WORK/partial.out" "make ${np} pending change" "partial summary counter disagrees with the WOULD lines"
chk "$WORK/partial.out" 'make 8 pending change' "partial stub plans other than 8 changes"

# ---- scenario: fully converged — the plan must be empty -------------------
GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=converged \
  bash "$SCRIPT" > "$WORK/converged.out" 2>&1 || { echo "FAIL: converged dry-run exited nonzero"; cat "$WORK/converged.out"; fails=1; }
absent "$WORK/converged.out" 'WOULD:' "converged state still plans changes"
chk    "$WORK/converged.out" 'ok: ruleset release-tags matches' "converged: ruleset not recognised"
chk    "$WORK/converged.out" 'ok: main branch protection matches' "converged: protection not recognised"
chk    "$WORK/converged.out" 'ok: private vulnerability reporting already enabled' "converged: PVR not recognised"
chk    "$WORK/converged.out" 'ok: environment release settings already match' "converged: release env not recognised"
chk    "$WORK/converged.out" 'ok: environment runtime-train settings already match' "converged: runtime-train env not recognised"
chk    "$WORK/converged.out" "ok: 'main' branch deployment policy present" "converged: runtime-train branch policy not recognised"
chk    "$WORK/converged.out" 'ok: sha_pinning_required already true' "converged: sha pinning not recognised"

# ---- scenario: single-field drift on a converged repo --------------------
# Each drift mode changes ONE managed field on one environment; the plan
# must contain exactly the repair for that drift and nothing else. The
# all-match no-call side of idempotency is the converged scenario above.
drift_case() {  # drift_case <GH_STUB_DRIFT> <expected WOULD pattern> <desc>
  local d="$1" out n
  out="$WORK/drift-$1.out"
  GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=converged GH_STUB_DRIFT="$d" \
    bash "$SCRIPT" > "$out" 2>&1 || { echo "FAIL: drift $d exited nonzero"; cat "$out"; fails=1; return; }
  chk "$out" "$2" "drift $d: $3"
  n="$(grep -c 'WOULD:' "$out")"
  [ "$n" -eq 1 ] || { echo "FAIL: drift $d plans $n change(s), want exactly 1"; fails=1; }
}
drift_case rel-psr    'WOULD: gh api -X PUT repos/test/repo/environments/release --input' \
  "prevent_self_review drift did not plan an env PUT"
chk "$WORK/drift-rel-psr.out" '"prevent_self_review": *false' "drift rel-psr: repair body wrong"
drift_case rel-wait   'WOULD: gh api -X PUT repos/test/repo/environments/release --input' \
  "wait_timer drift did not plan an env PUT"
chk "$WORK/drift-rel-wait.out" '"wait_timer": *0' "drift rel-wait: repair body wrong"
drift_case rel-dbp    'WOULD: gh api -X PUT repos/test/repo/environments/release --input' \
  "deployment_branch_policy drift did not plan an env PUT"
drift_case rel-pol    'WOULD: gh api -X DELETE repos/test/repo/environments/release/deployment-branch-policies/12' \
  "extra tag policy did not plan a DELETE"
drift_case train-psr  'WOULD: gh api -X PUT repos/test/repo/environments/runtime-train --input' \
  "prevent_self_review drift did not plan an env PUT"
chk "$WORK/drift-train-psr.out" '"prevent_self_review": *false' "drift train-psr: repair body wrong"
drift_case train-wait 'WOULD: gh api -X PUT repos/test/repo/environments/runtime-train --input' \
  "wait_timer drift did not plan an env PUT"
chk "$WORK/drift-train-wait.out" '"wait_timer": *0' "drift train-wait: repair body wrong"
drift_case train-dbp  'WOULD: gh api -X PUT repos/test/repo/environments/runtime-train --input' \
  "deployment_branch_policy drift did not plan an env PUT"
drift_case train-pol  'WOULD: gh api -X DELETE repos/test/repo/environments/runtime-train/deployment-branch-policies/22' \
  "extra branch policy did not plan a DELETE"
# rc==0 'missing' arm: GET 200 with the wanted entry absent -> POST it.
drift_case rel-pol-miss   'WOULD: gh api -X POST repos/test/repo/environments/release/deployment-branch-policies -f name=v\* -f type=tag' \
  "missing tag policy did not plan a POST"
drift_case train-pol-miss 'WOULD: gh api -X POST repos/test/repo/environments/runtime-train/deployment-branch-policies -f name=main -f type=branch' \
  "missing branch policy did not plan a POST"

# ---- scenario: malformed-success and unsafe drift must fail CLOSED -------
# An empty/garbage policies body or a partial page (total_count above the
# ?per_page=100 result) must abort, never report "ok". More than the
# DELETE bound of unexpected policies must abort for manual review.
fail_case() {  # fail_case <GH_STUB_DRIFT> <expected error pattern> <desc>
  local d="$1" out="$WORK/drift-$1.out"
  if GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=converged GH_STUB_DRIFT="$d" \
      bash "$SCRIPT" > "$out" 2>&1; then
    echo "FAIL: drift $d did not abort"; fails=1; return
  fi
  chk "$out" "$2" "drift $d: $3"
}
fail_case rel-pol-empty   'malformed body'   "empty policies body did not fail closed"
fail_case rel-pol-bad     'malformed body'   "non-JSON policies body did not fail closed"
fail_case rel-pol-pages   'partial page'     "paginated policies body did not fail closed"
fail_case rel-pol-many    'refusing to plan a mass deletion' "DELETE bound not enforced"
fail_case train-pol-empty 'malformed body'   "empty policies body did not fail closed"
fail_case train-pol-bad   'malformed body'   "non-JSON policies body did not fail closed"
fail_case train-pol-pages 'partial page'     "paginated policies body did not fail closed"
fail_case train-pol-many  'refusing to plan a mass deletion' "DELETE bound not enforced"

# ---- scenario: GET fails with a non-404 — must fail CLOSED ---------------
if GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=err500 \
    bash "$SCRIPT" > "$WORK/err.out" 2>&1; then
  echo "FAIL: HTTP 500 on environment GET did not abort"; fails=1
else
  chk "$WORK/err.out" 'refusing to continue' "non-404 GET did not fail closed"
fi
if GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=err500train \
    bash "$SCRIPT" > "$WORK/err-train.out" 2>&1; then
  echo "FAIL: HTTP 500 on runtime-train environment GET did not abort"; fails=1
else
  chk "$WORK/err-train.out" 'refusing to continue' "runtime-train GET did not fail closed"
fi
if GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=err500perms \
    bash "$SCRIPT" > "$WORK/err-perms.out" 2>&1; then
  echo "FAIL: HTTP 500 on actions/permissions GET did not abort"; fails=1
else
  chk "$WORK/err-perms.out" 'refusing to continue' "actions/permissions GET did not fail closed"
fi

# R1-39 / TM-49: the required contexts are exactly the ci.yml job names
# minus a documented list of network-dependent jobs, and every excluded
# job is named in the script header and in .github/README.md.
EXCLUDED=(
  "kasm adapter contract + catalog scan"
  "quickstart (kind up.sh + portal smoke)"
  "partitioned (kind up.sh + CHIPS cookie smoke)"
)
python3 -I - "$ROOT" "${EXCLUDED[@]}" <<'PY' || fails=1
import json, re, sys
try:
    import yaml
except ImportError:
    print("FAIL: PyYAML is not importable — cannot derive the ci.yml job set")
    sys.exit(1)
root, excluded = sys.argv[1], set(sys.argv[2:])
with open(f"{root}/.github/workflows/ci.yml") as fh:
    ci = yaml.safe_load(fh)
jobs = {b.get("name", k) for k, b in ci["jobs"].items()}
with open(f"{root}/.github/scripts/setup-repo-protection.sh") as fh:
    src = fh.read()
m = re.search(r"cat > \"\$TMP/protection\.json\" <<'EOF'\n(.*?)\nEOF\n", src, re.S)
if not m:
    print("FAIL: protection.json heredoc not found in setup-repo-protection.sh")
    sys.exit(1)
contexts = set(json.loads(m.group(1))["required_status_checks"]["contexts"])
bad = 0
for x in sorted(excluded - jobs):
    print(f"FAIL: documented exclusion '{x}' is not a ci.yml job name"); bad = 1
for x in sorted(jobs - excluded - contexts):
    print(f"FAIL: ci.yml job '{x}' is neither a required check nor a documented exclusion"); bad = 1
for x in sorted(contexts - jobs):
    print(f"FAIL: required context '{x}' is not a ci.yml job name"); bad = 1
for x in sorted(contexts & excluded):
    print(f"FAIL: network-dependent job '{x}' is required"); bad = 1
sys.exit(bad)
PY
for x in "${EXCLUDED[@]}"; do
  grep -qF -- "\"$x\"" "$SCRIPT" \
    || { echo "FAIL: setup-repo-protection.sh header does not name excluded job \"$x\""; fails=1; }
  grep -qF -- "\`$x\`" "$ROOT/.github/README.md" \
    || { echo "FAIL: .github/README.md does not name excluded job \`$x\`"; fails=1; }
done

if [ "$fails" -eq 0 ]; then echo "setup-repo-protection: all guards pass"; fi
exit "$fails"
