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
reply() { # reply <body>
  if [ -n "$jqexpr" ]; then printf '%s' "$1" | jq -r "$jqexpr"; else printf '%s\n' "$1"; fi
  exit 0
}
notfound() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
err500()   { echo "gh: boom (HTTP 500)" >&2; exit 1; }
mode="${GH_STUB_MODE:-clean}"
case "$endpoint" in
  repos/test/repo)
    [ "$mode" = "converged" ] && reply '{"visibility":"public","security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}'
    reply '{"visibility":"public","security_and_analysis":{}}' ;;
  user)                     reply '{"login":"octocat"}' ;;
  users/vanlongme)          reply '{"id":1234}' ;;
  repos/test/repo/environments/release)
    [ "$mode" = "err500" ] && err500
    case "$mode" in partial|converged) reply '{"protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"vanlongme","id":1234}}]}]}' ;; esac
    notfound ;;
  repos/test/repo/environments/release/deployment-branch-policies)
    case "$mode" in partial|converged) reply '{"total_count":1,"branch_policies":[{"name":"v*","type":"tag"}]}' ;; esac
    notfound ;;
  repos/test/repo/rulesets)
    case "$mode" in partial|converged) reply '[{"id":7,"name":"release-tags"}]' ;; esac
    reply '[]' ;;
  repos/test/repo/rulesets/7)
    [ "$mode" = "converged" ] && reply '{"id":7,"name":"release-tags","target":"tag","source_type":"Repository","enforcement":"active","bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}],"conditions":{"ref_name":{"exclude":[],"include":["refs/tags/v*"]}},"rules":[{"type":"creation"},{"type":"update"},{"type":"deletion"}],"created_at":"2026-01-01T00:00:00Z"}'
    reply '{"id":7,"name":"release-tags","target":"tag","enforcement":"evaluate","bypass_actors":[],"conditions":{"ref_name":{"exclude":[],"include":["refs/tags/v*"]}},"rules":[{"type":"deletion"}]}' ;;
  repos/test/repo/branches/main/protection)
    [ "$mode" = "partial" ] && reply '{}'
    [ "$mode" = "converged" ] && reply '{"url":"x","required_status_checks":{"strict":true,"contexts":["workflow lint (actionlint + yamllint + zizmor)","go vet + test -race (envtest)","integration (postgres service + envtest)","portal ui (npm ci, tsc, vitest, build)","chart (helm lint --strict + chart tests)","govulncheck (Go vuln scan)","dependency review (PRs)","workflow policy tests (.github)"],"checks":[]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"dismiss_stale_reviews":true,"require_code_owner_reviews":false,"required_approving_review_count":0},"required_linear_history":{"enabled":false},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"block_creations":{"enabled":false},"required_conversation_resolution":{"enabled":true},"lock_branch":{"enabled":false},"allow_fork_syncing":{"enabled":false}}'
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
chk "$WORK/clean.out" 'WOULD: gh api -X PATCH repos/test/repo' "security_and_analysis PATCH not planned"
chk "$WORK/clean.out" '"secret_scanning_push_protection"' "push protection not in PATCH body"
chk "$WORK/clean.out" 'WOULD: gh api -X POST repos/test/repo/actions/variables' "variables not planned"
chk "$WORK/clean.out" 'dry-run complete' "dry-run did not finish cleanly"

# ---- scenario: converged state -------------------------------------------
GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=partial \
  bash "$SCRIPT" > "$WORK/partial.out" 2>&1 || { echo "FAIL: partial dry-run exited nonzero"; cat "$WORK/partial.out"; fails=1; }
chk "$WORK/partial.out" 'ok: reviewers already match' "idempotent: reviewers re-detected wrongly"
chk "$WORK/partial.out" "ok: 'v\*' tag deployment policy present" "idempotent: tag policy re-detected wrongly"
chk "$WORK/partial.out" 'WOULD: gh api -X PUT repos/test/repo/rulesets/7' "idempotent: ruleset sync missing"
chk "$WORK/partial.out" 'ok: ATTESTATIONS_ENABLED already true' "idempotent: variable re-detected wrongly"
chk "$WORK/partial.out" 'WOULD: gh api -X PATCH repos/test/repo/actions/variables/CODE_SCANNING_ENABLED' "idempotent: stale variable not PATCHed"

chk "$WORK/partial.out" 'WOULD: gh api -X PUT repos/test/repo/branches/main/protection' "idempotent: protection drift not detected"

# ---- scenario: fully converged — the plan must be empty -------------------
GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=converged \
  bash "$SCRIPT" > "$WORK/converged.out" 2>&1 || { echo "FAIL: converged dry-run exited nonzero"; cat "$WORK/converged.out"; fails=1; }
absent "$WORK/converged.out" 'WOULD:' "converged state still plans changes"
chk    "$WORK/converged.out" 'ok: ruleset release-tags matches' "converged: ruleset not recognised"
chk    "$WORK/converged.out" 'ok: main branch protection matches' "converged: protection not recognised"
chk    "$WORK/converged.out" 'ok: private vulnerability reporting already enabled' "converged: PVR not recognised"

# ---- scenario: GET fails with a non-404 — must fail CLOSED ---------------
if GH_REPO=test/repo PATH="$WORK/bin:$PATH" GH_STUB_MODE=err500 \
    bash "$SCRIPT" > "$WORK/err.out" 2>&1; then
  echo "FAIL: HTTP 500 on environment GET did not abort"; fails=1
else
  chk "$WORK/err.out" 'refusing to continue' "non-404 GET did not fail closed"
fi

if [ "$fails" -eq 0 ]; then echo "setup-repo-protection: all guards pass"; fi
exit "$fails"
