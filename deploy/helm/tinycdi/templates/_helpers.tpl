{{/*
Helpers for the tinycdi chart.
*/}}

{{- define "tinycdi.name" -}}
tinycdi
{{- end }}

{{- define "tinycdi.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "tinycdi.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "tinycdi.labels" -}}
app.kubernetes.io/part-of: tinycdi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "tinycdi.componentLabels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/part-of: tinycdi
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{/*
Image reference. Accepts a dict {image: <images.* entry>, root: $}:
registry = image.registry | default global.imageRegistry; repository is
joined under it. digest wins over tag; tag defaults to Chart.AppVersion.
*/}}
{{- define "tinycdi.image" -}}
{{- $img := .image -}}
{{- $registry := $img.registry | default .root.Values.global.imageRegistry -}}
{{- $repo := $img.repository -}}
{{- if $registry -}}
{{- $repo = printf "%s/%s" ($registry | trimSuffix "/") $repo -}}
{{- end -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest }}
{{- else -}}
{{- $tag := $img.tag | default .root.Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag }}
{{- end -}}
{{- end }}

{{/* Per-image pullPolicy, default IfNotPresent. */}}
{{- define "tinycdi.imagePullPolicy" -}}
{{- . | default "IfNotPresent" }}
{{- end }}

{{/* Pod imagePullSecrets from global.imagePullSecrets (strings or maps). */}}
{{- define "tinycdi.imagePullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
imagePullSecrets:
  {{- range . }}
  {{- if kindIs "string" . }}
  - name: {{ . }}
  {{- else }}
  - {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- end }}
{{- end }}
{{- end }}

{{/* Comma-separated "tenant=namespace" pairs for TCDI_TENANT_NAMESPACES. */}}
{{- define "tinycdi.tenantNamespaces" -}}
{{- $pairs := list -}}
{{- range .Values.managedNamespaces -}}
{{- $pairs = append $pairs (printf "%s=%s" .tenant .name) -}}
{{- end -}}
{{- join "," $pairs }}
{{- end }}

{{/* JSON array for the backend -tenant-quotas flag: one entry per managedNamespaces
     entry that carries a quota block (tenant id = the entry's .tenant, exactly as
     tinycdi.tenantNamespaces maps it). Empty when no entry declares one. */}}
{{- define "tinycdi.tenantQuotas" -}}
{{- $quotas := list -}}
{{- range .Values.managedNamespaces -}}
{{- if .quota -}}
{{- $quotas = append $quotas (dict "tenant" .tenant "runningWorkspaces" .quota.runningWorkspaces "cpu" .quota.cpu "memory" .quota.memory "storage" .quota.storage) -}}
{{- end -}}
{{- end -}}
{{- if $quotas }}{{ toJson $quotas }}{{ end -}}
{{- end }}

{{/* Comma-separated tenant names for the backend TCDI_TENANT_ALLOWLIST (session gateway). */}}
{{- define "tinycdi.tenantAllowlist" -}}
{{- $names := list -}}
{{- range .Values.managedNamespaces -}}
{{- $names = append $names .tenant -}}
{{- end -}}
{{- join "," $names }}
{{- end }}

{{/*
Namespaces the operator cache watches: the managed namespaces ONLY. The
release namespace is deliberately absent (SEC-09) — no Workspace lives
there, and watching it would need secrets/pods RBAC in the platform
namespace. Binding the manager-role only in the managed namespaces matches
this list exactly.
*/}}
{{- define "tinycdi.watchNamespaces" -}}
{{- $ns := list -}}
{{- range .Values.managedNamespaces -}}
{{- $ns = append $ns .name -}}
{{- end -}}
{{- join "," $ns }}
{{- end }}

{{/* Hardened pod securityContext merged under user overrides. */}}
{{- define "tinycdi.podSecurityContext" -}}
{{- toYaml (mergeOverwrite (dict "runAsNonRoot" true "runAsUser" 65532 "runAsGroup" 65532 "seccompProfile" (dict "type" "RuntimeDefault")) (. | default dict)) }}
{{- end }}

{{/* Hardened container securityContext merged under user overrides. */}}
{{- define "tinycdi.containerSecurityContext" -}}
{{- toYaml (mergeOverwrite (dict "allowPrivilegeEscalation" false "readOnlyRootFilesystem" true "capabilities" (dict "drop" (list "ALL"))) (. | default dict)) }}
{{- end }}

{{/*
Edge ingress rule for the public listeners (backend app/session, frontend),
per networkPolicy.edgeIngress. Called with {root, ports}; renders a list
item for an `ingress:` list (nothing in cilium mode — a
CiliumNetworkPolicy admits the edge there, GitHub issue #13).
  ipBlock: from networkPolicy.edgeIngressCIDRs (default 0.0.0.0/0)
  any:     no `from` — every source, the TLS ports only
  cilium:  CiliumNetworkPolicy fromEntities [ingress, host, remote-node]
*/}}
{{- define "tinycdi.edgeIngressRule" -}}
{{- $mode := .root.Values.networkPolicy.edgeIngress | default "ipBlock" -}}
{{- if eq $mode "ipBlock" }}
- from:
    {{- range .root.Values.networkPolicy.edgeIngressCIDRs }}
    - ipBlock: {cidr: {{ . }}}
    {{- end }}
  ports:
    {{- range .ports }}
    - {protocol: TCP, port: {{ . }}}
    {{- end }}
{{- else if eq $mode "any" }}
# edgeIngress=any: every source, TLS ports only.
- ports:
    {{- range .ports }}
    - {protocol: TCP, port: {{ . }}}
    {{- end }}
{{- else }}
# edgeIngress=cilium: see the *-edge-ingress CiliumNetworkPolicy.
{{- end }}
{{- end }}

{{/* cert-manager bootstrap CA Secret name. */}}
{{- define "tinycdi.internalCASecret" -}}
{{- .Values.certManager.caSecretName | default (printf "%s-internal-ca" .Release.Name) }}
{{- end }}

{{/* Issuer the leaf Certificates reference. */}}
{{- define "tinycdi.leafIssuerRef" -}}
{{- if .Values.certManager.selfSigned -}}
kind: Issuer
name: {{ printf "%s-internal-ca" .Release.Name }}
{{- else -}}
kind: {{ .Values.certManager.issuerRef.kind }}
name: {{ .Values.certManager.issuerRef.name }}
{{- end -}}
{{- end }}

{{/*
Install-time invariants. Rendering FAILS when violated:

  - chart 0.2.0 replaced the api/gateway/portal components with backend
    and frontend: any api.*, gateway.*, portal.* or images.api/gateway/
    portal value fails with a migration hint (tinycdi.legacyValues).
    sessionHost (0.1.x) moved to sessionDomain — same hint.
  - portalHost must differ from sessionDomain and must NOT be inside it:
    the session edge serves every workspace on <label>.<sessionDomain>
    through the *.<sessionDomain> wildcard route, and a portalHost in
    that scope would be captured by the wildcard (portal cookies + OIDC
    redirect are origin-scoped). Compared with scheme/port stripped.
  - backend.sessionCookieMode is lax or partitioned; networkPolicy.
    edgeIngress is ipBlock, cilium or any (ipBlock needs edgeIngressCIDRs).
  - every managedNamespaces entry needs a non-empty name and tenant.
  - ingress.enabled and gatewayApi.enabled are mutually exclusive
    (single public exposure path).
  - certManager.enabled requires either selfSigned=true (the chart
    bootstraps a CA) or certManager.issuerRef.name pointing at an
    existing Issuer/ClusterIssuer.
  - SEC-32: an EMPTY peer list in a NetworkPolicy rule means "everywhere"
    — database.allowedPeers, oidc.egressCIDRs and (with backend metrics
    on) networkPolicy.prometheusPeers must never be empty. CHTR-7: the
    shipped 0.0.0.0/32 ipBlock in allowedPeers is a deny-all placeholder —
    it fails the render too.
  - SEC-35: edge TLS is mandatory — ingress.enabled requires
    ingress.tls.existingSecret; every gatewayApi parentRef (shared or
    per-route) must pin a sectionName (a TLS listener).
  - SEC-02: a seeded InternetOnly template requires operator.clusterCIDRs.
  - SEC-36/CHTR-2/CHTR-3: dev surfaces are gated behind dev.enabled —
    operator.devAllowNoBroker, dangerous extraArgs, hostPath extraVolumes,
    database.tls downgrade modes, podSecurity.managedEnforce=privileged
    and any securityContext override that weakens the hardened defaults.
  - SEC-06/37/CHTR-2: the node-profile installer lives in its OWN dedicated
    namespace — never the release namespace, a managed namespace, kube-*
    or default — and its image must be pinned.
*/}}
{{- /* tinycdi.durationNs renders a Go duration string ("5s", "1m30s",
        "1500ms", "0") as nanoseconds; an unparseable value fails the
        render naming the value (the binary would reject it at startup
        anyway — a render error beats a CrashLoopBackOff). */ -}}
{{- define "tinycdi.durationNs" -}}
{{- $v := printf "%v" . -}}
{{- if eq $v "0" -}}0{{- else -}}
{{- if not (regexMatch `^([0-9]*\.?[0-9]+(ns|us|µs|ms|s|m|h))+$` $v) -}}
{{- fail (printf "duration %q is not a Go duration (e.g. 5s, 1m30s, 1500ms, 0)" $v) -}}
{{- end -}}
{{- $seg := regexFindAll `[0-9]*\.?[0-9]+(ns|us|µs|ms|s|m|h)` $v -1 -}}
{{- if ne (join "" $seg) $v -}}
{{- fail (printf "duration %q has an unparseable residue" $v) -}}
{{- end -}}
{{- $unitNs := dict "ns" 1.0 "us" 1000.0 "µs" 1000.0 "ms" 1000000.0 "s" 1000000000.0 "m" 60000000000.0 "h" 3600000000000.0 -}}
{{- $ns := 0.0 -}}
{{- range $s := $seg -}}
{{- $u := regexFind `[a-zµ]+$` $s -}}
{{- $n := float64 (regexReplaceAll `[a-zµ]+$` $s "") -}}
{{- $ns = addf $ns (mulf $n (index $unitNs $u)) -}}
{{- end -}}
{{- printf "%.0f" $ns -}}
{{- end -}}
{{- end -}}

{{- define "tinycdi.validate" -}}
{{- include "tinycdi.legacyValues" . -}}
{{- $portal := .Values.portalHost | default "" | trim -}}
{{- $session := .Values.sessionDomain | default "" | trim -}}
{{- if eq $portal "" -}}
{{- fail "portalHost is required (e.g. portal.example.com)" -}}
{{- end -}}
{{- if eq $session "" -}}
{{- fail "sessionDomain is required (e.g. session.example.com — every workspace session is served on <label>.<sessionDomain>)" -}}
{{- end -}}
{{- $ph := regexReplaceAll ":[0-9]+$" (regexReplaceAll "^[a-zA-Z]+://" $portal "") "" | lower -}}
{{- $sh := regexReplaceAll ":[0-9]+$" (regexReplaceAll "^[a-zA-Z]+://" $session "") "" | lower -}}
{{- if or (eq $ph $sh) (hasSuffix (printf ".%s" $sh) $ph) -}}
{{- fail (printf "portalHost (%[1]q) must differ from sessionDomain and must not be inside it (%[2]q) — the *.%[2]s wildcard route would capture portal traffic" $ph $sh) -}}
{{- end -}}
{{- /* backend.extraAllowedHosts fed the removed -session-allowed-hosts:
        the session Host allowlist is now the session domain itself plus
        the in-cluster control hosts. */ -}}
{{- if .Values.backend.extraAllowedHosts -}}
{{- fail "backend.extraAllowedHosts moved to backend.controlHosts — the session listener serves <label>.<sessionDomain> workspace hosts and answers /healthz + /v1/control/* only on the control hosts (in-cluster Service names)" -}}
{{- end -}}
{{- if not (has (printf "%v" .Values.backend.sessionCookieMode) (list "lax" "partitioned")) -}}
{{- fail (printf "backend.sessionCookieMode must be lax or partitioned (got %q)" (printf "%v" .Values.backend.sessionCookieMode)) -}}
{{- end -}}
{{- /* FX-R34 drain budget: the propagation wait plus the drain window
        must leave >= 4 s of the backend's 24 s shared shutdown deadline
        for listener shutdown (the binary enforces the same bound at
        startup — fail the render instead of a CrashLoopBackOff). */ -}}
{{- $drainNs := addf (float64 (include "tinycdi.durationNs" (.Values.backend.drainPropagationDelay | default "5s"))) (float64 (include "tinycdi.durationNs" (.Values.backend.drainWindow | default "8s"))) -}}
{{- if gt $drainNs 20000000000.0 -}}
{{- fail (printf "backend.drainPropagationDelay + backend.drainWindow must be <= 20s so listener shutdown fits the 24s shutdown deadline (got %s + %s)" (printf "%v" .Values.backend.drainPropagationDelay) (printf "%v" .Values.backend.drainWindow)) -}}
{{- end -}}
{{- /* FX-R35 drain budget: the frontend's drain delay must leave >= 10 s
        of its 30 s terminationGracePeriodSeconds for graceful listener
        shutdown — fail the render, not the pod. */ -}}
{{- if gt (float64 (include "tinycdi.durationNs" (.Values.frontend.drainDelay | default "8s"))) 20000000000.0 -}}
{{- fail (printf "frontend.drainDelay must be <= 20s so graceful shutdown fits the 30s terminationGracePeriodSeconds (got %s)" (printf "%v" .Values.frontend.drainDelay)) -}}
{{- end -}}
{{- range .Values.managedNamespaces -}}
{{- if or (not .name) (not .tenant) -}}
{{- fail "every managedNamespaces entry needs non-empty name and tenant" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.ingress.enabled .Values.gatewayApi.enabled -}}
{{- fail "ingress.enabled and gatewayApi.enabled are mutually exclusive — pick ONE public exposure path" -}}
{{- end -}}
{{- if and .Values.certManager.enabled (not .Values.certManager.selfSigned) (not .Values.certManager.issuerRef.name) -}}
{{- fail "certManager.enabled requires issuerRef.name (existing Issuer/ClusterIssuer) or selfSigned=true" -}}
{{- end -}}
{{- /* SEC-32: empty peer lists would render `to: []`/`from: []` = anywhere. */ -}}
{{- if .Values.networkPolicy.enabled -}}
{{- /* CHTR-7: the shipped 0.0.0.0/32 ipBlock is a deny-all placeholder —
        leaving it in place silently cuts the api from its database; an
        empty list is just as wrong. A peer only counts when it is not the
        placeholder. */ -}}
{{- $realDBPeer := false -}}
{{- range $p := (.Values.database.allowedPeers | default (list)) -}}
{{- $cidr := get (default dict (get (default dict $p) "ipBlock")) "cidr" -}}
{{- if ne (printf "%v" $cidr) "0.0.0.0/32" -}}{{- $realDBPeer = true -}}{{- end -}}
{{- end -}}
{{- if not $realDBPeer -}}
{{- fail "database.allowedPeers must name real database peers — the shipped ipBlock 0.0.0.0/32 is a deny-all placeholder and an empty list opens egress everywhere" -}}
{{- end -}}
{{- if not .Values.oidc.egressCIDRs -}}
{{- fail "oidc.egressCIDRs must not be empty when networkPolicy.enabled — an empty list opens egress everywhere; set the IdP CIDRs" -}}
{{- end -}}
{{- if and .Values.backend.metrics.enabled (not .Values.networkPolicy.prometheusPeers) -}}
{{- fail "networkPolicy.prometheusPeers must not be empty when backend.metrics.enabled — scope metrics scraping to your monitoring pods" -}}
{{- end -}}
{{- $edge := .Values.networkPolicy.edgeIngress | default "ipBlock" -}}
{{- if not (has $edge (list "ipBlock" "cilium" "any")) -}}
{{- fail (printf "networkPolicy.edgeIngress must be ipBlock, cilium or any (got %q)" $edge) -}}
{{- end -}}
{{- if and (eq $edge "ipBlock") (not .Values.networkPolicy.edgeIngressCIDRs) -}}
{{- fail "networkPolicy.edgeIngressCIDRs must not be empty when edgeIngress=ipBlock — an empty peer list admits everything; use edgeIngress=any to say so explicitly" -}}
{{- end -}}
{{- end -}}
{{- /* D20: the app listener seals OIDC login state into the
        __Host-tcdi_login cookie with an AEAD key — required so a login
        started on one replica completes on another. */ -}}
{{- if not (or .Values.backend.loginKeys.existingSecret .Values.backend.loginKeys.generate) -}}
{{- fail "backend.loginKeys is required: set existingSecret (a Secret with keys \"current\" and optional \"previous\", 32 bytes each) or generate=true to let the chart create it once" -}}
{{- end -}}
{{- /* SEC-35: edge TLS is mandatory. */ -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.tls.existingSecret) -}}
{{- fail "ingress.tls.existingSecret is required when ingress.enabled — edge TLS is mandatory" -}}
{{- end -}}
{{- if .Values.gatewayApi.enabled -}}
{{- /* A route resolves to its non-empty per-route list, else the shared
        gatewayApi.parentRefs — every rendered route needs >= 1 ref. */ -}}
{{- $portalPRs := or .Values.gatewayApi.portalParentRefs .Values.gatewayApi.parentRefs -}}
{{- $sessionPRs := or .Values.gatewayApi.sessionParentRefs .Values.gatewayApi.parentRefs -}}
{{- if or (not $portalPRs) (not $sessionPRs) -}}
{{- fail "gatewayApi.enabled requires at least one parentRef per route — set gatewayApi.parentRefs or the per-route portalParentRefs/sessionParentRefs" -}}
{{- end -}}
{{- range concat $portalPRs $sessionPRs -}}
{{- if not .sectionName -}}
{{- fail "every gatewayApi parentRef entry needs sectionName naming a TLS listener — a bare parentRef binds every listener including plain HTTP" -}}
{{- end -}}
{{- end -}}
{{- /* Two HTTPRoutes sharing one metadata.name collide on apply. */ -}}
{{- if eq .Values.gatewayApi.portalRouteName .Values.gatewayApi.sessionRouteName -}}
{{- fail "gatewayApi.portalRouteName and gatewayApi.sessionRouteName must differ — two HTTPRoutes cannot share one metadata.name" -}}
{{- end -}}
{{- end -}}
{{- /* SEC-02: InternetOnly templates need the cluster CIDRs declared. */ -}}
{{- $internetOnly := false -}}
{{- range .Values.templates -}}
{{- if eq (get (default dict .spec) "networkProfile") "InternetOnly" -}}
{{- $internetOnly = true -}}
{{- end -}}
{{- end -}}
{{- if and $internetOnly (not .Values.operator.clusterCIDRs) -}}
{{- fail "operator.clusterCIDRs is required when a seeded template uses networkProfile=InternetOnly — declare this cluster's pod/service/node CIDRs so runtime egress excludes them" -}}
{{- end -}}
{{- /* The Kasm adapter is opt-in (KASM-2 condition 1): the operator gets
        --kasm-adapter-image ONLY when kasmAdapter.enabled=true, however the
        digest got into the values (release stamping fills it in the
        packaged chart). Enabled needs a digest-pinned image — a mutable tag
        would let an image swap change what runs inside every kasm pod —
        and a seeded adapter=kasm template needs the adapter enabled (the
        backend rejects it when the operator has no --kasm-adapter-image). */ -}}
{{- $kasmSeed := false -}}
{{- range .Values.templates -}}
{{- if eq (printf "%v" (get (default dict (get (default dict .spec) "linux")) "adapter")) "kasm" -}}
{{- $kasmSeed = true -}}
{{- end -}}
{{- end -}}
{{- $kaImage := (default dict (get (default dict .Values.kasmAdapter) "image")) -}}
{{- $kaEnabled := (default dict .Values.kasmAdapter).enabled -}}
{{- if and $kasmSeed (not $kaEnabled) -}}
{{- fail "kasmAdapter.enabled=true is required when a seeded template uses spec.linux.adapter=kasm — the operator only gets --kasm-adapter-image when the adapter is enabled" -}}
{{- end -}}
{{- if and $kaEnabled (not $kaImage.digest) -}}
{{- fail "kasmAdapter.enabled=true requires kasmAdapter.image.digest — the adapter init image must be digest-pinned (a tag alone is refused)" -}}
{{- end -}}
{{- if and $kaEnabled (not $kaImage.repository) -}}
{{- fail "kasmAdapter.enabled=true requires kasmAdapter.image.repository" -}}
{{- end -}}
{{- if and (not $kaImage.digest) $kaImage.tag -}}
{{- fail "kasmAdapter.image.tag without .digest is refused — --kasm-adapter-image accepts digest-pinned refs only" -}}
{{- end -}}
{{- /* E13: a seeded adapter=kasm template with experience=Browser may only
        use an image on kasmAdapter.browserAllowlist — allowlist membership
        is the chart-side binding to the catalog's ≤2-major browser engine
        gate (check-kasm-catalog.sh keeps the list ⊆ browser-class
        entries). Non-Browser kasm templates are unaffected (the ≤4 gate). */ -}}
{{- $browserAllow := (default list (get (default dict .Values.kasmAdapter) "browserAllowlist")) -}}
{{- range .Values.templates -}}
{{- $s := default dict .spec -}}
{{- $l := default dict (get $s "linux") -}}
{{- if and (eq (printf "%v" (get $l "adapter")) "kasm") (eq (get $s "experience") "Browser") -}}
{{- $img := (get $l "image") | default .image -}}
{{- if and $img (hasKey $.Values.images $img) -}}
{{- $img = include "tinycdi.image" (dict "image" (index $.Values.images $img) "root" $) -}}
{{- end -}}
{{- if not (has $img $browserAllow) -}}
{{- fail (printf "templates[%s]: adapter=kasm + experience=Browser requires the image on kasmAdapter.browserAllowlist (E13: only browser-class catalog entries inside the ≤2-major engine gate qualify — %q is not allowlisted)" .name $img) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* SEC-36: dev/privileged surfaces are gated behind dev.enabled. */ -}}
{{- if not .Values.dev.enabled -}}
{{- if .Values.operator.devAllowNoBroker -}}
{{- fail "operator.devAllowNoBroker requires dev.enabled=true — never set it in a production values file" -}}
{{- end -}}
{{- /* CHTR-3: extraArgs must not reopen the surfaces the chart pins closed
        (the operator's dev/no-broker and egress-except flags and its
        metrics endpoint, the backend's DB-TLS escape hatch, login gate,
        chart-managed metrics listener and the split/test-mode broker
        client flags). */ -}}
{{- include "tinycdi.denyExtraArgs" (dict "component" "operator" "args" .Values.operator.extraArgs "denied" (list "dev-allow-no-broker" "disable-builtin-egress-excepts" "metrics-bind-address" "metrics-secure" "metrics-cert-path" "metrics-cert-name" "metrics-cert-key")) -}}
{{- include "tinycdi.denyExtraArgs" (dict "component" "backend" "args" .Values.backend.extraArgs "denied" (list "dev-insecure-db" "required-groups" "metrics-listen" "broker-url" "broker-ca" "mtls-cert" "mtls-key")) -}}
{{- /* A hostPath extraVolume would mount the host filesystem into the
        session edge (CHTR-3). */ -}}
{{- range $v := (.Values.backend.extraVolumes | default (list)) -}}
{{- if hasKey (default dict $v) "hostPath" -}}
{{- fail "backend.extraVolumes must not use hostPath without dev.enabled=true — it mounts the host filesystem into the session edge" -}}
{{- end -}}
{{- end -}}
{{- /* DB TLS: only verifying sslmodes are production values (CHTR-3/8 —
        the api refuses to start on disable/allow/prefer/require unless
        -dev-insecure-db). */ -}}
{{- if not (has (printf "%v" .Values.database.tls.mode) (list "verify-ca" "verify-full")) -}}
{{- fail "database.tls.mode must be verify-ca or verify-full — weaker modes (\"\", prefer, require, allow, disable) require dev.enabled=true" -}}
{{- end -}}
{{- /* CHTR-2: privileged PSS on the managed namespaces reopens
        operator→privileged-pod — dev only. */ -}}
{{- if eq (printf "%v" .Values.podSecurity.managedEnforce) "privileged" -}}
{{- fail "podSecurity.managedEnforce=privileged requires dev.enabled=true — managed namespaces must stay baseline/restricted in production" -}}
{{- end -}}
{{- range $c := list "backend" "operator" "frontend" -}}
{{- $cv := default dict (index $.Values $c) -}}
{{- include "tinycdi.noPrivilegedOverride" (dict "component" $c "sc" (index $cv "securityContext") "kind" "securityContext") -}}
{{- include "tinycdi.noPrivilegedOverride" (dict "component" $c "sc" (index $cv "podSecurityContext") "kind" "podSecurityContext") -}}
{{- end -}}
{{- end -}}
{{- /* SEC-06/37/CHTR-2: the installer lives in its own dedicated
        namespace (never the release namespace, a managed namespace,
        kube-* or default — it must not share a namespace with platform,
        tenant or system workloads) and its image must be pinned. */ -}}
{{- if .Values.nodeProfiles.install.enabled -}}
{{- $np := .Values.nodeProfiles.install -}}
{{- $nsBad := or (eq $np.namespace $.Release.Namespace) (eq $np.namespace "default") (hasPrefix "kube-" $np.namespace) -}}
{{- range $.Values.managedNamespaces -}}
{{- if eq .name $np.namespace -}}{{- $nsBad = true -}}{{- end -}}
{{- end -}}
{{- if $nsBad -}}
{{- fail "nodeProfiles.install.namespace must be a dedicated namespace — never the release namespace, a managed namespace, kube-* or default (the installer needs PSS=privileged and must not share it with platform, tenant or system workloads)" -}}
{{- end -}}
{{- if and (not $np.image.digest) (not $np.image.tag) -}}
{{- fail "nodeProfiles.install.image needs a tag or digest — there is no :latest fallback" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
0.2.0 migration guard: the api, gateway and portal components were
replaced by backend (public API + session gateway in one deployment) and
frontend (static SPA server), and the single sessionHost became the
sessionDomain wildcard. Old keys are accepted by the schema only so
this can fail with a pointer to the new key instead of a bare schema error.
*/}}
{{- define "tinycdi.legacyValues" -}}
{{- $hints := dict
  "api" "api.* moved to backend.* (api.internalTLS -> backend.tls.internal, api.clientCA/extraCA/operatorCN/sessionIdle/extraPortalOrigins -> backend.*)"
  "gateway" "gateway.* moved to backend.* (gateway.tls -> backend.tls.session, gateway.metricsListen -> backend.metrics, gateway.id -> backend.gatewayID, gateway.mtls/trustedCA removed — the gateway reaches the broker in-process; gateway.service removed — expose the session host through ingress or gatewayApi)"
  "portal" "portal.* moved to frontend.* (portal.tls -> frontend.tls; portal.apiUpstream removed — the edge routes /v1 to the backend; portal.service removed — expose the portal host through ingress or gatewayApi)" -}}
{{- $found := list -}}
{{- range $k := list "api" "gateway" "portal" -}}
{{- if hasKey $.Values $k -}}{{- $found = append $found (get $hints $k) -}}{{- end -}}
{{- end -}}
{{- range $k := list "api" "gateway" "portal" -}}
{{- if hasKey (default dict $.Values.images) $k -}}
{{- $found = append $found (printf "images.%s moved to images.%s.*" $k (ternary "frontend" "backend" (eq $k "portal"))) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $.Values "sessionHost" -}}
{{- $found = append $found "sessionHost moved to sessionDomain (D9) — every workspace session is served on its own <label>.<sessionDomain> host; the edge needs one wildcard route and a wildcard certificate for *.<sessionDomain>" -}}
{{- end -}}
{{- if $found -}}
{{- fail (printf "chart 0.2.0 replaced the api, gateway and portal components with backend and frontend — migrate these values (see the chart README \"Upgrading to 0.2.0\"): %s" (join "; " $found)) -}}
{{- end -}}
{{- end }}

{{/*
CHTR-3 helper: refuses extraArgs entries whose flag name matches a denied
list (the flag name is the entry with leading dashes stripped, up to the
first `=` or whitespace — both `--flag=v` and `--flag v` forms are caught).
Called with {component, args, denied}.
*/}}
{{- define "tinycdi.denyExtraArgs" -}}
{{- range $arg := (.args | default (list)) -}}
{{- $name := regexReplaceAll "^-+" (printf "%v" $arg) "" -}}
{{- $name = regexReplaceAll "[=\\s].*$" $name "" -}}
{{- if has $name $.denied -}}
{{- fail (printf "%s.extraArgs entry %q reopens a surface the chart pins closed — it requires dev.enabled=true" $.component $arg) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
SEC-36 helper: refuses securityContext overrides that would weaken the
hardened defaults unless dev.enabled. Called with {component, sc, kind}.
*/}}
{{- define "tinycdi.noPrivilegedOverride" -}}
{{- $sc := .sc | default dict -}}
{{- $bad := list -}}
{{- if eq (printf "%v" (get $sc "privileged")) "true" -}}{{- $bad = append $bad "privileged" -}}{{- end -}}
{{- if eq (printf "%v" (get $sc "allowPrivilegeEscalation")) "true" -}}{{- $bad = append $bad "allowPrivilegeEscalation" -}}{{- end -}}
{{- if eq (printf "%v" (get $sc "readOnlyRootFilesystem")) "false" -}}{{- $bad = append $bad "readOnlyRootFilesystem=false" -}}{{- end -}}
{{- if eq (printf "%v" (get $sc "runAsNonRoot")) "false" -}}{{- $bad = append $bad "runAsNonRoot=false" -}}{{- end -}}
{{- if eq (printf "%v" (get $sc "runAsUser")) "0" -}}{{- $bad = append $bad "runAsUser=0" -}}{{- end -}}
{{- if eq (printf "%v" (get $sc "runAsGroup")) "0" -}}{{- $bad = append $bad "runAsGroup=0" -}}{{- end -}}
{{- if eq (printf "%v" (get $sc "procMount")) "Unmasked" -}}{{- $bad = append $bad "procMount=Unmasked" -}}{{- end -}}
{{- /* hostUsers=false (a user namespace for the pod) is a HARDENING and is
        allowed; hostUsers=true is the k8s default anyway. (CHTR-3) */ -}}
{{- if eq (printf "%v" (get $sc "fsGroup")) "0" -}}{{- $bad = append $bad "fsGroup=0" -}}{{- end -}}
{{- range $g := (get $sc "supplementalGroups" | default (list)) -}}{{- if eq (printf "%v" $g) "0" -}}{{- $bad = append $bad "supplementalGroups=[0]" -}}{{- end -}}{{- end -}}
{{- if and (hasKey $sc "seccompProfile") (eq (printf "%v" (get (get $sc "seccompProfile") "type")) "Unconfined") -}}{{- $bad = append $bad "seccompProfile=Unconfined" -}}{{- end -}}
{{- /* CHTR-3: an Unconfined AppArmor profile or any seLinuxOptions
        assignment escape confinement entirely. appArmorProfile is a map
        ({type: ...}) but a bare string is rejected too. */ -}}
{{- if eq (printf "%v" (get $sc "appArmorProfile")) "Unconfined" -}}{{- $bad = append $bad "appArmorProfile=Unconfined" -}}{{- end -}}
{{- if kindIs "map" (get $sc "appArmorProfile") -}}{{- if eq (printf "%v" (get (get $sc "appArmorProfile") "type")) "Unconfined" -}}{{- $bad = append $bad "appArmorProfile=Unconfined" -}}{{- end -}}{{- end -}}
{{- if and (hasKey $sc "seLinuxOptions") (get $sc "seLinuxOptions") -}}{{- $bad = append $bad "seLinuxOptions" -}}{{- end -}}
{{- /* capabilities.add widens; a user-set capabilities.drop that omits
        ALL silently REPLACES the merged drop list (CHTR-3). */ -}}
{{- if hasKey $sc "capabilities" -}}
{{- $caps := get $sc "capabilities" -}}
{{- if not (kindIs "map" $caps) -}}
{{- $bad = append $bad "capabilities (not a map)" -}}
{{- else -}}
{{- if get $caps "add" -}}{{- $bad = append $bad "capabilities.add" -}}{{- end -}}
{{- if and (hasKey $caps "drop") (not (has "ALL" (get $caps "drop" | default (list)))) -}}{{- $bad = append $bad "capabilities.drop without ALL" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- if $bad -}}
{{- fail (printf "%s.%s weakens the hardened securityContext (%s) — requires dev.enabled=true" .component .kind (join ", " $bad)) -}}
{{- end -}}
{{- end }}
