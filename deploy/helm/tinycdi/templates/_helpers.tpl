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

{{/* Comma-separated tenant names for the gateway tenant-allowlist. */}}
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

  - portalHost and sessionHost must resolve to DIFFERENT registrable hosts
    (portal cookies + OIDC redirect are origin-scoped; sharing a host would
    let session traffic ride the portal's cookie scope). Compared with
    scheme/port stripped.
  - every managedNamespaces entry needs a non-empty name and tenant.
  - ingress.enabled and gatewayApi.enabled are mutually exclusive
    (single public exposure path).
  - certManager.enabled requires either selfSigned=true (the chart
    bootstraps a CA) or certManager.issuerRef.name pointing at an
    existing Issuer/ClusterIssuer.
  - SEC-32: an EMPTY peer list in a NetworkPolicy rule means "everywhere"
    — database.allowedPeers, oidc.egressCIDRs and (with gateway metrics
    on) networkPolicy.prometheusPeers must never be empty. CHTR-7: the
    shipped 0.0.0.0/32 ipBlock in allowedPeers is a deny-all placeholder —
    it fails the render too.
  - SEC-35: edge TLS is mandatory — ingress.enabled requires
    ingress.tls.existingSecret; every gatewayApi parentRef must pin a
    sectionName (a TLS listener).
  - SEC-02: a seeded InternetOnly template requires operator.clusterCIDRs.
  - SEC-36/CHTR-2/CHTR-3: dev surfaces are gated behind dev.enabled —
    operator.devAllowNoBroker, dangerous extraArgs, hostPath extraVolumes,
    database.tls downgrade modes, podSecurity.managedEnforce=privileged
    and any securityContext override that weakens the hardened defaults.
  - SEC-06/37/CHTR-2: the node-profile installer lives in its OWN dedicated
    namespace — never the release namespace, a managed namespace, kube-*
    or default — and its image must be pinned.
*/}}
{{- define "tinycdi.validate" -}}
{{- $portal := .Values.portalHost | default "" | trim -}}
{{- $session := .Values.sessionHost | default "" | trim -}}
{{- if eq $portal "" -}}
{{- fail "portalHost is required (e.g. portal.example.com)" -}}
{{- end -}}
{{- if eq $session "" -}}
{{- fail "sessionHost is required (e.g. session.example.com)" -}}
{{- end -}}
{{- $ph := regexReplaceAll ":[0-9]+$" (regexReplaceAll "^[a-zA-Z]+://" $portal "") "" | lower -}}
{{- $sh := regexReplaceAll ":[0-9]+$" (regexReplaceAll "^[a-zA-Z]+://" $session "") "" | lower -}}
{{- if eq $ph $sh -}}
{{- fail (printf "portalHost and sessionHost must be DIFFERENT registrable hosts (both resolve to %q)" $ph) -}}
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
{{- if and .Values.gateway.metricsListen (not .Values.networkPolicy.prometheusPeers) -}}
{{- fail "networkPolicy.prometheusPeers must not be empty when gateway.metricsListen is set — scope metrics scraping to your monitoring pods" -}}
{{- end -}}
{{- end -}}
{{- /* SEC-35: edge TLS is mandatory. */ -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.tls.existingSecret) -}}
{{- fail "ingress.tls.existingSecret is required when ingress.enabled — edge TLS is mandatory" -}}
{{- end -}}
{{- if .Values.gatewayApi.enabled -}}
{{- if not .Values.gatewayApi.parentRefs -}}
{{- fail "gatewayApi.enabled requires at least one parentRef" -}}
{{- end -}}
{{- range .Values.gatewayApi.parentRefs -}}
{{- if not .sectionName -}}
{{- fail "every gatewayApi.parentRefs entry needs sectionName naming a TLS listener — a bare parentRef binds every listener including plain HTTP" -}}
{{- end -}}
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
{{- /* SEC-36: dev/privileged surfaces are gated behind dev.enabled. */ -}}
{{- if not .Values.dev.enabled -}}
{{- if .Values.operator.devAllowNoBroker -}}
{{- fail "operator.devAllowNoBroker requires dev.enabled=true — never set it in a production values file" -}}
{{- end -}}
{{- /* CHTR-3: extraArgs must not reopen the surfaces the chart pins closed
        (the operator's dev/no-broker and egress-except flags and its
        metrics endpoint, the api's DB-TLS escape hatch and login gate,
        the gateway's chart-managed metrics listener). */ -}}
{{- include "tinycdi.denyExtraArgs" (dict "component" "operator" "args" .Values.operator.extraArgs "denied" (list "dev-allow-no-broker" "disable-builtin-egress-excepts" "metrics-bind-address" "metrics-secure" "metrics-cert-path" "metrics-cert-name" "metrics-cert-key")) -}}
{{- include "tinycdi.denyExtraArgs" (dict "component" "api" "args" .Values.api.extraArgs "denied" (list "dev-insecure-db" "required-groups")) -}}
{{- include "tinycdi.denyExtraArgs" (dict "component" "gateway" "args" .Values.gateway.extraArgs "denied" (list "metrics-listen")) -}}
{{- /* A hostPath extraVolume would mount the host filesystem into the
        session edge (CHTR-3). */ -}}
{{- range $v := (.Values.gateway.extraVolumes | default (list)) -}}
{{- if hasKey (default dict $v) "hostPath" -}}
{{- fail "gateway.extraVolumes must not use hostPath without dev.enabled=true — it mounts the host filesystem into the session edge" -}}
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
{{- range $c := list "api" "operator" "gateway" "portal" -}}
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
