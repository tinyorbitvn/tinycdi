{{/*
Chart-rendered container args, one helper per component (TM-42).
The Deployment templates include these, and tinycdi.renderedFlagNames
reads the flag names back so <component>.extraArgs can never repeat
one: extraArgs renders after these and the last value of a flag wins.
Each helper renders the arg ENTRIES (a YAML list), never the `args:` key.
Every chart-rendered flag belongs here (TestExtraArgsCannotOverrideRenderedFlags).
*/}}

{{- define "tinycdi.args.backend" -}}
{{- $revokedSerials := ((.Values.backend.tls.internal | default dict).revokedSerials) | default dict -}}
{{- $loginSecret := .Values.backend.loginKeys.existingSecret | default (printf "%s-backend-login-keys" .Release.Name) -}}
{{- $loginPrevious := false -}}
{{- with lookup "v1" "Secret" $.Release.Namespace $loginSecret -}}
{{- if hasKey (default dict .data) "previous" -}}
{{- $loginPrevious = true -}}
{{- end -}}
{{- end -}}
# app listener: public API for the portal host.
- -listen=:8443
- -tls-cert=/tls/app/tls.crt
- -tls-key=/tls/app/tls.key
- -session-idle={{ .Values.backend.sessionIdle }}
# readiness drop -> endpoint-removal propagation wait ->
# graceful 1001 stream close -> drain-window hold (FX-R34).
- -drain-propagation-delay={{ .Values.backend.drainPropagationDelay }}
- -drain-window={{ .Values.backend.drainWindow }}
{{- $tenantQuotas := include "tinycdi.tenantQuotas" . }}
{{- if $tenantQuotas }}
# managedNamespaces[].quota: the singleton leader upserts these
# tenant_quota rows at startup; unlisted tenants are untouched.
- {{ printf "-tenant-quotas=%s" $tenantQuotas | quote }}
{{- end }}
{{- $tenantQuotaCeilings := include "tinycdi.tenantQuotaCeilings" . }}
{{- if $tenantQuotaCeilings }}
# managedNamespaces[].quotaCeiling: the highest quota a tenant
# admin may set through the admin quota API.
- {{ printf "-tenant-quota-ceilings=%s" $tenantQuotaCeilings | quote }}
{{- end }}
{{- if .Values.oidc.requiredGroups }}
- --required-groups={{ join "," .Values.oidc.requiredGroups }}
{{- end }}
# Tenant-admin role group(s) — the rendered flag keeps the
# configured set visible in the manifest.
- --tenant-admin-groups={{ join "," .Values.oidc.tenantAdminGroups }}
# Sign-out also ends the IdP session (RP-initiated logout).
- --oidc-end-session={{ ne (toString .Values.oidc.endSession) "false" }}
{{- if .Values.oidc.postLogoutRedirect }}
- --oidc-post-logout-redirect={{ .Values.oidc.postLogoutRedirect }}
{{- end }}
{{- if and .Values.dev.enabled (not (has (printf "%v" .Values.database.tls.mode) (list "verify-ca" "verify-full"))) }}
- --dev-insecure-db
{{- end }}
# AEAD keys sealing the __Host-tcdi_login cookie (D20): the
# FIRST file seals, every file opens — "previous" carries the
# pre-rotation key while it exists in the Secret (in-cluster
# lookup decides; under helm template/GitOps add
# "-login-key-file=/login-keys/previous" via backend.extraArgs
# during a rotation).
- -login-key-file=/login-keys/current
{{- if $loginPrevious }}
- -login-key-file=/login-keys/previous
{{- end }}
# session listener: session gateway on every workspace host
# <label>.<sessionDomain> (D9).
- -session-listen=:8444
- -session-tls-cert=/tls/session/tls.crt
- -session-tls-key=/tls/session/tls.key
# /healthz and the bearer-gated /v1/control/* answer ONLY on
# these Hosts — the in-cluster Service names — never on a
# workspace host (D9); backend.controlHosts adds extras (the
# allowlist compares the hostname sans port).
- -session-control-hosts={{ join "," (concat (list "backend" (printf "backend.%s" .Release.Namespace) (printf "backend.%s.svc" .Release.Namespace) (printf "backend.%s.svc.cluster.local" .Release.Namespace)) (.Values.backend.controlHosts | default (list))) }}
# lax requires portal and session on the same registrable
# domain; partitioned (SameSite=None; Secure; Partitioned) is
# for cross-site deployments.
- -session-cookie-mode={{ .Values.backend.sessionCookieMode }}
# Per-workspace bound on live upstream connections through
# this replica (V11): the runtime's fd-driven crash bound is
# ~330 conns; the cap converts a storm into a retryable 429.
- -session-stream-cap={{ .Values.backend.sessionStreamCap | int }}
# Release the lease of an attached session idle this long (no
# request, stream or transfer in flight) so a closed tab stops
# pinning the workspace; 0 disables.
- -idle-session-release={{ .Values.backend.idleSessionRelease }}
{{- if .Values.backend.upstreamCA.existingSecret }}
- -upstream-ca=/etc/upstream-ca/{{ .Values.backend.upstreamCA.key | default "ca.crt" }}
{{- end }}
{{- if .Values.backend.controlToken.existingSecret }}
- -control-token-file=/control/{{ .Values.backend.controlToken.key | default "token" }}
{{- end }}
# internal listener: operator mTLS broker API (ADR 0003).
- -internal-listen=:9443
- -internal-tls-cert=/tls/internal/tls.crt
- -internal-tls-key=/tls/internal/tls.key
- -internal-client-ca=/etc/client-ca/ca.crt
{{- if $revokedSerials.existingSecret }}
# ADR 0008: per-certificate revocation — the Secret's
# blocklist key is projected to a fixed filename.
- -internal-revoked-list=/etc/internal-revoked/revoked-serials.txt
{{- end }}
{{- if .Values.backend.operatorCN }}
- -operator-cn={{ .Values.backend.operatorCN }}
{{- end }}
{{- if .Values.backend.metrics.enabled }}
- -metrics-listen=:{{ .Values.backend.metrics.port | int }}
{{- end }}
# shared: the session domain names the per-workspace hosts
# (launch target + ticket audience); the portal origins are
# the API CSRF Origin allowlist, the launch Origin allowlist
# (ADR 0004) and the session listener's frame-ancestors.
- -session-domain={{ .Values.sessionDomain }}
{{- range prepend (.Values.backend.extraPortalOrigins | default (list)) (printf "https://%s" .Values.portalHost) }}
- -portal-origin={{ . }}
{{- end }}
- -gateway-audience={{ .Values.backend.audience | default .Values.sessionDomain }}
# ONE identity for the whole Deployment (D19/D22): the lease
# directory is shared, so a per-pod id would split ownership
# across replicas. Literal value — never the pod name.
- -gateway-id={{ .Values.backend.gatewayID }}
{{- if .Values.backend.trustedProxies }}
# CIDRs whose X-Forwarded-For claims are trusted (E7/TM-32):
# rate limits and the runtime-bound headers derive the client
# from the right-most untrusted chain entry. Required behind
# an ingress/Gateway — empty shares one bucket per edge.
- -trusted-proxies={{ join "," .Values.backend.trustedProxies }}
{{- end }}
# Rate-limit per-replica ceiling (RL-1, ADR 0006): the shared
# Postgres window enforces -login-rate/-launch-rate as the
# exact aggregate; each pod additionally keeps its budget
# divided by this count as the local ceiling — the cap while
# Postgres is healthy and the fail-open limit during a store
# outage. Clamped to >= 1 — backend.replicas may be 0 (scaled
# down), and a later out-of-band scale-up must not crash-loop
# on a 0 divisor. Rendered before extraArgs so an override
# there (e.g. an HPA's maxReplicas) wins.
- -rate-limit-replicas={{ max 1 (.Values.backend.replicas | int) }}
# Terminal-row retention (SR-4-F5): the leader's hourly sweep
# deletes connection_lease/launch_ticket rows terminal for
# longer than this — 0 disables and the tables grow unbounded.
- -terminal-retention-days={{ .Values.backend.retention.terminalRowDays }}
# Audit retention (ADR 0010): the leader's daily prune.
- -audit-retention-days={{ .Values.backend.retention.auditDays }}
# Platform per-file bound for session file transfer (ADR 0009
# §5); a tenant ceiling may only lower it. int64 keeps the
# value decimal — big integers render in scientific notation.
- -file-transfer-max-bytes={{ int64 .Values.backend.fileTransfer.maxFileBytes }}
{{- end }}

{{- define "tinycdi.args.operator" -}}
- --health-probe-bind-address=:8081
# Metrics stay OFF (SEC-33): controller-runtime's secure mode
# also enables the authn/authz filter, which needs
# cluster-scoped TokenReview/SAR RBAC this chart never grants;
# plain-HTTP metrics are not offered either.
- --metrics-bind-address=0
- --webhook-port={{ .Values.operator.webhookPort }}
- --leader-elect={{ .Values.operator.leaderElect }}
- --leader-election-namespace={{ .Release.Namespace }}
- --watch-namespaces={{ include "tinycdi.watchNamespaces" . }}
{{- $except := concat (.Values.operator.internetExceptCIDRs | default (list)) (.Values.operator.clusterCIDRs | default (list)) }}
{{- if $except }}
- --internet-except-cidrs={{ join "," $except }}
{{- end }}
{{- if .Values.operator.brokerClient.enabled }}
# Internal broker (backend mTLS listener): the operator client
# cert CN is the only identity the workspace revoke/drain routes
# accept (ADR 0003; default CN=operator, backend.operatorCN
# overrides).
- --broker-internal-url={{ .Values.operator.brokerClient.internalURL | default (printf "https://backend.%s.svc:9443" .Release.Namespace) }}
- --broker-ca-file=/etc/broker-ca/{{ .Values.operator.brokerClient.ca.key | default "ca.crt" }}
- --broker-client-cert-file=/mtls/tls.crt
- --broker-client-key-file=/mtls/tls.key
{{- end }}
{{- if (default dict .Values.kasmAdapter).enabled }}
# Adapter init image for spec.linux.adapter=kasm templates
# (docs/kasm-images.md). Rendered ONLY when kasmAdapter.enabled
# (a release-stamped digest alone does not turn it on) and then
# always digest-pinned — validation.yaml refuses enabled without
# a digest, and the operator refuses a tag-only ref at startup.
- --kasm-adapter-image={{ include "tinycdi.image" (dict "image" .Values.kasmAdapter.image "root" $) }}
{{- end }}
{{- $placement := (default dict (default dict .Values.runtime).placement) }}
{{- if not ($placement.allowSharedNodes | default false) }}
# Dedicated workspace pool (runtime.placement, default): every
# runtime pod selects the pool label and tolerates its taint.
# validation.yaml refuses allowSharedNodes=false with an empty
# selector, so these args always carry a real constraint.
- {{ printf "--runtime-node-selector=%s" ($placement.nodeSelector | toJson) | quote }}
- {{ printf "--runtime-tolerations=%s" ($placement.tolerations | default list | toJson) | quote }}
{{- end }}
{{- if not (kindIs "invalid" .Values.runtime.hostUsers) }}
- --runtime-host-users={{ .Values.runtime.hostUsers }}
{{- end }}
{{- /* The operator flag defaults to true, so only false renders. */}}
{{- if eq (printf "%v" (dig "appArmor" "requireRuntimeDefault" true .Values.runtime)) "false" }}
- --runtime-apparmor-require-default=false
{{- end }}
{{- /* Same shape: --runtime-topology-spread defaults to true. */}}
{{- if eq (printf "%v" (dig "topologySpread" "enabled" true .Values.runtime)) "false" }}
- --runtime-topology-spread=false
{{- end }}
{{- /* ClusterOnly egress posture: the restricted default
       (tenant-labelled namespaces only) renders NO flag.
       unrestricted is the opt-out literal; the egress*
       values render the narrowing JSON — the two never mix. */}}
{{- $co := (dig "networkProfiles" "clusterOnly" dict (default dict .Values.runtime)) }}
{{- $coEgress := dict }}
{{- if $co.egressNamespaceSelector }}{{- $_ := set $coEgress "namespaceSelector" $co.egressNamespaceSelector }}{{- end }}
{{- if $co.egressPodSelector }}{{- $_ := set $coEgress "podSelector" $co.egressPodSelector }}{{- end }}
{{- if $co.egressPorts }}{{- $_ := set $coEgress "ports" $co.egressPorts }}{{- end }}
{{- if and $co.unrestricted $coEgress }}
{{- fail "clusterOnly.unrestricted cannot be combined with egress selectors" }}
{{- end }}
{{- if $co.unrestricted }}
- --runtime-clusteronly-egress=unrestricted
{{- else if $coEgress }}
- {{ printf "--runtime-clusteronly-egress=%s" (toJson $coEgress) | quote }}
{{- end }}
{{- if .Values.operator.devAllowNoBroker }}
- --dev-allow-no-broker
{{- end }}
{{- end }}

{{- define "tinycdi.args.frontend" -}}
{{- $brandingCM := get (default dict .Values.frontend.branding) "configMap" -}}
- -listen=:8443
# pre-stop drain (FX-R35): /healthz fails on SIGTERM and the
# server keeps serving this long while the endpoint removal
# propagates, then shuts down gracefully.
- -drain-delay={{ .Values.frontend.drainDelay }}
# The SPA shows sessions in an iframe on each workspace's own
# <label>.<sessionDomain> host and launches them with a form
# POST there — the CSP frame-src and form-action get the
# https://*.<sessionDomain> wildcard or every session is
# CSP-blocked (D14).
- -session-domain={{ .Values.sessionDomain }}
- -tls-cert=/tls/tls.crt
- -tls-key=/tls/tls.key
{{- if .Values.frontend.webRoot }}
- -web-root={{ .Values.frontend.webRoot }}
{{- end }}
{{- if $brandingCM }}
- -branding-dir=/branding
{{- end }}
{{- end }}
