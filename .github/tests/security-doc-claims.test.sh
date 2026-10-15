#!/usr/bin/env bash
# security-doc-claims.test.sh — pins the security claims in prose that the
# v1.0 security review (docs/security/review-v1.0.md) corrected. Each line
# names its review finding and states what a document must say (need), must
# not say again (absent), or what a threat-model register row must carry
# (need_row). Files are flattened to one line with every whitespace run
# squeezed to one space, so a claim may wrap anywhere in the source.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TM="docs/security/threat-model.md"
fails=0

flat() { tr -s '[:space:]' ' ' < "$ROOT/$1"; }

need() { # need <finding> <repo-relative file> <ERE>
  if [ ! -f "$ROOT/$2" ]; then
    echo "FAIL: $1: $2 does not exist"; fails=$((fails + 1)); return
  fi
  if ! flat "$2" | grep -qE -- "$3"; then
    echo "FAIL: $1: $2 does not state /$3/"; fails=$((fails + 1))
  fi
}

absent() { # absent <finding> <repo-relative file> <ERE>
  if [ ! -f "$ROOT/$2" ]; then
    echo "FAIL: $1: $2 does not exist"; fails=$((fails + 1)); return
  fi
  if flat "$2" | grep -qE -- "$3"; then
    echo "FAIL: $1: $2 still states /$3/"; fails=$((fails + 1))
  fi
}

need_row() { # need_row <finding> <TM-NN> <ERE>
  local row
  row="$(grep -E "^\| $2 \|" "$ROOT/$TM")"
  if [ -z "$row" ]; then
    echo "FAIL: $1: $TM has no register row $2"; fails=$((fails + 1)); return
  fi
  if ! printf '%s\n' "$row" | grep -qE -- "$3"; then
    echo "FAIL: $1: register row $2 does not state /$3/"; fails=$((fails + 1))
  fi
}

# R1-17: a removed tenant admin keeps the role until the next sign-in; the
# docs must say so and name the immediate remedy.
need_row R1-17 TM-11 'next sign-in'
need_row R1-17 TM-11 'users/\{ownerRef\}/sessions:revoke-all'
need R1-17 SECURITY.md 'admin group at the IdP takes effect at their next sign-in'
need R1-17 SECURITY.md 'users/\{ownerRef\}/sessions:revoke-all'

# R1-21: the server does not require a dry run before a confirmed :apply.
need_row R1-21 TM-17 'the API requires an explicit `dryRun` flag'
absent R1-21 "$TM" 'it runs a dry run first'
absent R1-21 internal/api/openapi.yaml 'Always dry run first'
need R1-21 internal/api/openapi.yaml 'The server does not require one'

# R1-27: the in-pod trust residual names the desktop leg everywhere it is stated.
need_row R1-27 TM-37 'desktop leg'
need_row R1-27 TM-37 'bind an unmodified runtime only'
need     R1-27 docs/security/threat-model.md 'TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition'
need     R1-27 docs/releases/v1.0.0.md 'over the desktop leg'
absent   R1-27 docs/releases/v1.0.0.md 'Net effect: a tampered helper can mishandle only'
need     R1-27 docs/runbooks/session-policy.md 'over the desktop leg'
need     R1-27 SECURITY.md 'Clipboard, file-transfer and other in-desktop restrictions are UX defaults, not DLP'

# R1-07: a workspace's process/thread count is bounded only by the kubelet's
# podPidsLimit; the install guide requires it, preflight checks it, the
# quickstart cluster runs bounded, and the residual is stated.
need R1-07 docs/runbooks/install.md 'podPidsLimit'
need R1-07 docs/runbooks/install.md 'kernel\.threads-max'
need R1-07 hack/quickstart/kind-config.yaml 'kind: KubeletConfiguration podPidsLimit: [1-9][0-9]*'
need R1-07 hack/preflight/README.md '\| `pids` \|'
need R1-07 docs/releases/v1.0.0.md 'Process count needs a kubelet bound\.'
need R1-07 docs/security/threat-model.md 'Process count needs a kubelet bound\.'
need R1-07 docs/security/threat-model.md '\*\*Pod hardening\*\*:[^*]*podPidsLimit'

need R1-29 docs/security/threat-model.md '\*\*Pod hardening\*\*:[^*]*create user, PID and network namespaces[^*]*hostUsers: false'
need_row R1-29 TM-47 'chromium-userns\.json` the workspace user can create user, PID and network namespaces'
need R1-29 SECURITY.md 'chromium-userns\.json` lets the workspace user create user, PID and network namespaces'
need R1-29 deploy/node-profiles/README.md 'must run its pods with `hostUsers: false`'
need R1-29 deploy/helm/tinycdi/README.md 'chromium-userns\.json` needs an effective `false`'
need R1-29 docs/releases/v1.0.0.md 'The browser sandbox profile allows namespaces\.'
need R1-29 deploy/node-profiles/README.md 'a custom `Localhost` profile that also allows namespace creation is not detected'
need_row R1-29 TM-47 'a custom `Localhost` profile that allows namespace creation is not detected'

# R1-15: trusted-proxy guidance never trusts a workspace pod range; TM-34
# says the unrestricted opt-out exposes the backend edge to workspaces.
need R1-15 docs/runbooks/install.md '[Nn]ever list the cluster pod CIDR or any range where workspace pods run'
absent R1-15 docs/runbooks/install.md 'list the cluster pod CIDR \(shown'
need R1-15 deploy/helm/tinycdi/README.md '[Nn]ever list the cluster pod CIDR'
absent R1-15 deploy/helm/tinycdi/README.md 'pod CIDR containing the traefik pods'
need_row R1-15 TM-34 'edgeIngressCIDRs.*admits workspace pods'
need_row R1-15 TM-34 'backend\.trustedProxies` must never cover'
# R1-36: no dead podSecurity value is documented.
absent R1-36 deploy/helm/tinycdi/README.md 'platformEnforce'
absent R1-36 deploy/helm/tinycdi/values.yaml 'platformEnforce'

exit "$fails"
