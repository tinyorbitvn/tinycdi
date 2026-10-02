// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package linux is the Kubernetes Pod backend for LinuxContainer workspaces
// (design §4/§5/§6/§7). It converges five child objects, all
// named and labeled with the Workspace UID:
//
//	Pod           ws-<uid>           non-root, drop ALL, no privesc, seccomp,
//	                                 readinessProbe exec healthcheck, bounded
//	                                 /dev/shm emptyDir counted in memory
//	Service       ws-<uid>           ClusterIP :8443, selector = workspace UID
//	Secret        ws-<uid>-rt        per-workspace runtime credential
//	                                 (username/password/tls) generated here,
//	                                 never logged or written to status
//	PVC           ws-<uid>-home      only when dataPolicy=Retain; carries NO
//	                                 ownerReference (retention contract)
//	NetworkPolicy ws-<uid>-boundary  default-deny, ingress only from gateway
//	                                 pods in the platform namespace, egress =
//	                                 cluster DNS (+ profile rules); the spec
//	                                 is reconciled, not create-only
//
// A same-named object whose workspace-uid label does not match is never
// adopted: Ensure/Stop report ErrNameConflict instead.
package linux

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/runtime"
)

// Labels every runtime child carries. The workspace UID is part of every
// child name and label set so nothing is ever adopted across workspaces.
const (
	LabelWorkspaceUID      = "workspaces.cdi.tinyorbit.vn/workspace-uid"
	LabelWorkspaceName     = "workspaces.cdi.tinyorbit.vn/workspace-name"
	LabelRuntimeGeneration = "workspaces.cdi.tinyorbit.vn/runtime-generation"
	LabelRole              = "workspaces.cdi.tinyorbit.vn/role"
	LabelDataRole          = "workspaces.cdi.tinyorbit.vn/data-role"
	LabelDataRetained      = "workspaces.cdi.tinyorbit.vn/data-retained"
	LabelPartOf            = "app.kubernetes.io/part-of"

	RoleRuntime = "runtime"
	RoleGateway = "gateway"
	PartOf      = "tinycdi"

	// RuntimeServiceAccount is the dedicated per-namespace SA the runtime
	// pod runs under — the chart creates it in every managed namespace
	// with automountServiceAccountToken=false. The pod spec still pins
	// automount=false explicitly so identity never depends on the SA's
	// settings (SEC-30).
	RuntimeServiceAccount = "tinycdi-runtime"

	// AnnotationNodeSelector is an optional WorkspaceTemplate annotation:
	// a JSON map[string]string applied to the runtime Pod's nodeSelector.
	// Deprecated: spec.placement.nodeSelector is the typed replacement and
	// wins when both are set; the annotation ships for one release (D24).
	AnnotationNodeSelector = "workspaces.cdi.tinyorbit.vn/node-selector"

	// AnnotationSeccompProfile is an optional WorkspaceTemplate annotation
	// selecting the container seccomp profile. Only "localhost/<name>" is
	// honored (the browser-sandbox Localhost profile); anything else is
	// ignored and RuntimeDefault applies.
	AnnotationSeccompProfile = "workspaces.cdi.tinyorbit.vn/seccomp-profile"

	// AnnotationAppArmorProfile is an optional WorkspaceTemplate annotation
	// selecting the container AppArmor profile (the K8s 1.30+
	// securityContext.appArmorProfile field). Only "localhost/<name>" — a
	// profile pre-loaded on the node — and "runtime/default" are accepted;
	// anything else (including "unconfined") rejects the template with
	// ErrTemplateRejected rather than silently weakening confinement.
	AnnotationAppArmorProfile = "workspaces.cdi.tinyorbit.vn/apparmor-profile"

	// AnnotationStorageClass is an optional WorkspaceTemplate annotation
	// naming the StorageClass for the Retain home PVC.
	AnnotationStorageClass = "workspaces.cdi.tinyorbit.vn/storage-class"

	// AnnotationRetainedPVC marks a Workspace CR created by attach
	// (design §5): the workspace's home volume is an EXISTING retained PVC
	// mounted by reference — the annotation value is the claim name. The
	// backend must never create a fresh volume in its place.
	AnnotationRetainedPVC = "workspaces.cdi.tinyorbit.vn/retained-pvc"
	// AnnotationRetainedPVCUID records the retained claim's UID alongside
	// AnnotationRetainedPVC: dataset identity is PVC UID + workspace UID,
	// never the reusable name.
	AnnotationRetainedPVCUID = "workspaces.cdi.tinyorbit.vn/retained-pvc-uid"

	streamingPort int32 = 8443

	secretMountDir = "/run/secrets/tcdi"
	runtimeDir     = "/run/tcdi"
	homeDir        = "/home/workspace"
	shmDir         = "/dev/shm"

	// Adapter contract (adapter=kasm, docs/kasm-images.md): the operator's
	// --kasm-adapter-image runs as an initContainer whose /install-adapter
	// writes the adapter scripts into the shared adapterDir emptyDir; the
	// desktop container mounts it read-only and runs adapterEntrypoint.
	adapterVolName    = "tcdi-adapter"
	adapterDir        = "/opt/tcdi"
	adapterEntrypoint = "/opt/tcdi/entrypoint.sh"
	adapterInitName   = "tcdi-adapter-init"
	adapterInstaller  = "/install-adapter"

	// KASM-1: the adapter volume also carries browser-shim.sh, bind-mounted
	// read-only (subPath) over every Chromium-family wrapper path the
	// kasmweb/* images ship. Those wrappers hardcode --no-sandbox; every
	// relaunch surface (desktop icon, menu, xdg-open, the x-www-browser /
	// sensible-browser alternatives chain) lands on one of them. The shim
	// execs the real binary with sandbox-killing flags stripped. subPath
	// targets that don't exist in a given image are created by the
	// kubelet, so one list covers the whole Chromium family.
	adapterShimSubPath = "browser-shim.sh"

	// KASM-8: chromium-policy.json is mounted (subPath, read-only) into
	// each engine's managed-policy directory. It sorts last
	// alphabetically so it overrides the image's permissive files
	// (CommandLineFlagSecurityWarningsEnabled=false, unrestricted
	// DevTools/extensions, Safe Browsing off).
	adapterPolicySubPath = "chromium-policy.json"
)

// AdapterBrowserWrappers are the Chromium-family launcher paths the shim
// is mounted over. Only WRAPPER paths belong here — never the real
// binary (the shim execs it; shadowing it would loop).
var AdapterBrowserWrappers = []string{
	"/usr/bin/chromium",
	"/usr/bin/chromium-browser",
	"/usr/bin/chromium-browser-stable",
	"/usr/bin/google-chrome",
	"/usr/bin/google-chrome-stable",
	"/usr/bin/microsoft-edge",
	"/usr/bin/microsoft-edge-stable",
	"/usr/bin/brave-browser",
	"/usr/bin/brave-browser-stable",
	"/usr/bin/vivaldi",
	"/usr/bin/vivaldi-stable",
	"/usr/bin/opera",
	// Kasm replaces /usr/bin/x-www-browser with a real --no-sandbox
	// wrapper script (it is NOT the usual alternatives symlink); the
	// x-www-browser entry under /etc/alternatives points at
	// /usr/bin/chromium, already covered.
	"/usr/bin/x-www-browser",
}

// AdapterPolicyTargets are the managed-policy destinations per engine
// (debian chromium, google-chrome, msedge). Missing parents are created
// by the kubelet; engines without the dir simply ignore the file.
var AdapterPolicyTargets = []string{
	"/etc/chromium/policies/managed/zz-tcdi.json",
	"/etc/opt/chrome/policies/managed/zz-tcdi.json",
	"/etc/opt/edge/policies/managed/zz-tcdi.json",
}

// CRUID is a Workspace CR's metadata.uid — the identity every runtime
// child object name (and every child workspace-uid label value) derives
// from. It is a DIFFERENT identity from the platform workspace id
// (provisioning.PlatformID, "ws_…") the API mints and the broker routes
// on: passing one where the other is expected is a compile error. The
// alias keeps ws.UID assignable without casts.
type CRUID = types.UID

// Deterministic child names — CRUID-scoped, well under 63 chars (UID is 36).
func PodName(uid CRUID) string     { return "ws-" + string(uid) }
func ServiceName(uid CRUID) string { return "ws-" + string(uid) }
func SecretName(uid CRUID) string  { return "ws-" + string(uid) + "-rt" }
func PVCName(uid CRUID) string     { return "ws-" + string(uid) + "-home" }
func NetPolName(uid CRUID) string  { return "ws-" + string(uid) + "-boundary" }

// pvcNameFor returns the home claim for ws: an attached workspace mounts
// the retained PVC referenced by its annotations; every other workspace
// uses its deterministic own-volume name.
func pvcNameFor(ws *workspacesv1alpha1.Workspace) string {
	if ref := ws.Annotations[AnnotationRetainedPVC]; ref != "" {
		return ref
	}
	return PVCName(ws.UID)
}

// ErrNameConflict is returned when a same-named child exists but is owned by
// a different workspace UID. The foreign object is left untouched.
var ErrNameConflict = errors.New("linux backend: refusing to adopt foreign-owned object")

type conflictError struct {
	kind string
	name string
}

func (e *conflictError) Error() string {
	return fmt.Sprintf("%v: %s %s", ErrNameConflict, e.kind, e.name)
}
func (e *conflictError) Is(target error) bool { return target == ErrNameConflict }

// ErrTemplateRejected is returned when a WorkspaceTemplate requests a
// runtime configuration outside policy (today: an apparmor-profile value
// other than "localhost/<name>" or "runtime/default"). Ensure fails before
// any child object is created; the operator surfaces the reason on the
// Workspace's Degraded condition.
var ErrTemplateRejected = errors.New("linux backend: template rejected")

type templateRejectedError struct {
	reason string
}

func (e *templateRejectedError) Error() string {
	return fmt.Sprintf("%v: %s", ErrTemplateRejected, e.reason)
}
func (e *templateRejectedError) Is(target error) bool { return target == ErrTemplateRejected }

// Options tunes the backend. The zero value is safe (Isolated-egress,
// RuntimeDefault seccomp).
type Options struct {
	// InternetExceptCIDRs are extra CIDRs subtracted from the 0.0.0.0/0
	// allow under NetworkProfileInternetOnly — the cluster pod/service/node
	// ranges, which differ per install (network-isolation fixture shape,
	// design §6). They are always applied ON TOP of builtinEgressExcepts.
	InternetExceptCIDRs []string

	// DisableBuiltinEgressExcepts drops builtinEgressExcepts from the
	// InternetOnly except list (--disable-builtin-egress-excepts). An
	// explicit break-glass: without the built-ins an InternetOnly workspace
	// can reach RFC1918/CGNAT/loopback/link-local ranges — on flat CNIs
	// that includes the rest of the cluster.
	DisableBuiltinEgressExcepts bool

	// GatewayNamespace is the platform namespace the session gateways run
	// in; runtime ingress admits pods labeled role=gateway ONLY from there.
	// Empty fails closed to the workspace's own namespace — a same-
	// namespace gateway still works, but a labeled pod anywhere else is
	// never admitted. cmd/operator defaults it to POD_NAMESPACE.
	GatewayNamespace string

	// KasmAdapterImage is the digest-pinned image reference for the
	// adapter initContainer used by templates with spec.linux.adapter=kasm
	// (docs/kasm-images.md). It comes from operator configuration
	// (--kasm-adapter-image), never from the template. Empty means kasm
	// templates are rejected (ErrTemplateRejected) — the adapter can never
	// fall back to a mutable or caller-chosen image.
	KasmAdapterImage string

	// DefaultPlacement is the operator-wide pod placement applied when a
	// template sets neither the spec.placement field nor (for nodeSelector)
	// the deprecated node-selector annotation. Per field the precedence is
	// template → annotation → this default; a field the template sets
	// replaces the default outright.
	DefaultPlacement workspacesv1alpha1.PlacementSpec

	// DefaultHostUsers is the operator-wide pod hostUsers value applied
	// when spec.linux.hostUsers is unset. Nil leaves the pod field nil
	// (the apiserver default — host user namespace).
	DefaultHostUsers *bool

	// AppArmorNotRequired (operator --runtime-apparmor-require-default=false)
	// makes buildPod leave securityContext.appArmorProfile nil wherever it
	// would have set RuntimeDefault, so runtime pods start on nodes without
	// AppArmor (kind, SELinux-based distributions). A Localhost profile
	// requested through the template annotation is always kept. The zero
	// value keeps today's behaviour: RuntimeDefault is always set.
	AppArmorNotRequired bool
}

// builtinEgressExcepts are always subtracted from the 0.0.0.0/0 allow of
// NetworkProfileInternetOnly: loopback, RFC1918, CGNAT, link-local (cloud
// metadata), multicast and reserved space are never "the internet" —
// and on Canal/Calico-style CNIs the 0.0.0.0/0 ipBlock otherwise matches
// cluster pod, node and service IPs too (SEC-02). The policy only grants
// IPv4 destinations, so no IPv6 excepts are needed: every v6 packet is
// already denied for lack of a matching rule.
var builtinEgressExcepts = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

// egressExcepts computes the InternetOnly except list: built-ins unless
// opted out, then the configured cluster CIDRs, deduplicated.
func egressExcepts(opts Options) []string {
	var out []string
	seen := map[string]bool{}
	add := func(cidrs []string) {
		for _, c := range cidrs {
			if c = strings.TrimSpace(c); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	if !opts.DisableBuiltinEgressExcepts {
		add(builtinEgressExcepts)
	}
	add(opts.InternetExceptCIDRs)
	return out
}

// Backend implements runtime.Backend for LinuxContainer workspaces.
type Backend struct {
	client client.Client
	opts   Options
}

var _ runtime.Backend = (*Backend)(nil)

// New returns a Linux Pod backend.
func New(c client.Client, opts Options) *Backend {
	return &Backend{client: c, opts: opts}
}

func labels(ws *workspacesv1alpha1.Workspace) map[string]string {
	return map[string]string{
		LabelWorkspaceUID:  string(ws.UID),
		LabelWorkspaceName: ws.Name,
		LabelRole:          RoleRuntime,
		LabelPartOf:        PartOf,
	}
}

// checkOwned verifies a fetched child belongs to this workspace.
func checkOwned(kind string, obj metav1.Object, uid CRUID) error {
	if obj.GetLabels()[LabelWorkspaceUID] != string(uid) {
		return &conflictError{kind: kind, name: obj.GetName()}
	}
	return nil
}

// Ensure converges the runtime for ws toward the spec captured by tpl and
// returns the observation of the incarnation it converged.
func (b *Backend) Ensure(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (runtime.Observation, error) {
	// Policy gate: a template requesting an out-of-policy runtime profile
	// is rejected before ANY child object is created — a half-converged
	// workspace that can never become valid must not exist.
	appArmor, err := resolveAppArmorProfile(tpl)
	if err != nil {
		return runtime.Observation{}, err
	}
	if tpl.Spec.Linux != nil && tpl.Spec.Linux.Adapter == workspacesv1alpha1.AdapterKasm &&
		b.opts.KasmAdapterImage == "" {
		// The adapter is delivered by the operator-configured init image;
		// without it the pod cannot satisfy the runtime contract, so the
		// template is rejected before ANY child object is created.
		return runtime.Observation{}, &templateRejectedError{reason: "spec.linux.adapter=kasm requires the operator's --kasm-adapter-image (digest-pinned adapter init image)"}
	}
	if _, err := b.ensureSecret(ctx, ws); err != nil {
		return runtime.Observation{}, err
	}
	if ws.Spec.DataPolicy == workspacesv1alpha1.DataPolicyRetain {
		if _, err := b.ensurePVC(ctx, ws, tpl); err != nil {
			return runtime.Observation{}, err
		}
	}
	if err := b.ensurePod(ctx, ws, tpl, appArmor); err != nil {
		return runtime.Observation{}, err
	}
	if err := b.ensureService(ctx, ws); err != nil {
		return runtime.Observation{}, err
	}
	if err := b.ensureNetPol(ctx, ws, tpl); err != nil {
		return runtime.Observation{}, err
	}
	return b.Observe(ctx, ws)
}

// Observe reports the current incarnation without changing anything.
func (b *Backend) Observe(ctx context.Context, ws *workspacesv1alpha1.Workspace) (runtime.Observation, error) {
	uid := ws.UID
	obs := runtime.Observation{RuntimeGeneration: ws.Spec.RuntimeGeneration}

	pod := &corev1.Pod{}
	podErr := b.client.Get(ctx, client.ObjectKey{Name: PodName(uid), Namespace: ws.Namespace}, pod)
	switch {
	case apierrors.IsNotFound(podErr):
		obs.Reason = "Provisioning"
	case podErr != nil:
		return obs, podErr
	default:
		if pod.Labels[LabelWorkspaceUID] != string(uid) {
			// foreign squatter: not our incarnation; report conflict.
			return obs, &conflictError{kind: "Pod", name: pod.Name}
		}
		obs.RuntimeUID = string(pod.UID)
		if g := pod.Labels[LabelRuntimeGeneration]; g != "" {
			fmt.Sscanf(g, "%d", &obs.RuntimeGeneration)
		}
		obs.RuntimeReady = podReady(pod)
		obs.Reason = podReason(pod)
	}

	svc := &corev1.Service{}
	svcErr := b.client.Get(ctx, client.ObjectKey{Name: ServiceName(uid), Namespace: ws.Namespace}, svc)
	switch {
	case apierrors.IsNotFound(svcErr):
	case svcErr != nil:
		return obs, svcErr
	default:
		obs.ServiceRef = &workspacesv1alpha1.ServiceReference{Name: svc.Name, Port: streamingPort}
	}
	// ConnectionReady: the streaming endpoint answers when the runtime is
	// ready and the fronting Service exists (endpoints are populated by the
	// control plane from the same Ready signal).
	obs.ConnectionReady = obs.RuntimeReady && obs.ServiceRef != nil

	if ws.Spec.DataPolicy == workspacesv1alpha1.DataPolicyRetain {
		pvc := &corev1.PersistentVolumeClaim{}
		pvcErr := b.client.Get(ctx, client.ObjectKey{Name: pvcNameFor(ws), Namespace: ws.Namespace}, pvc)
		switch {
		case apierrors.IsNotFound(pvcErr):
			obs.StorageReady = false
		case pvcErr != nil:
			return obs, pvcErr
		default:
			obs.StorageReady = pvc.Status.Phase == corev1.ClaimBound
		}
	} else {
		// Ephemeral home is an emptyDir — bound by definition once the pod
		// object exists.
		obs.StorageReady = true
	}
	return obs, nil
}

// Stop terminates the current incarnation but keeps data allowed by
// dataPolicy. Service/Secret/NetworkPolicy stay; they are cheap and the
// (runtimeGeneration, runtimeUID) fencing rejects stale connections anyway.
func (b *Backend) Stop(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	uid := ws.UID
	pod := &corev1.Pod{}
	err := b.client.Get(ctx, client.ObjectKey{Name: PodName(uid), Namespace: ws.Namespace}, pod)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if err := checkOwned("Pod", pod, uid); err != nil {
		return err
	}
	if pod.DeletionTimestamp.IsZero() {
		return b.client.Delete(ctx, pod)
	}
	return nil
}

// DeleteRuntime removes the runtime and its ephemeral child objects. A Retain
// home PVC is deliberately left behind (retention contract); an Ephemeral
// PVC, if present, is removed.
func (b *Backend) DeleteRuntime(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	uid := ws.UID

	del := func(kind string, obj client.Object) error {
		err := b.client.Get(ctx, client.ObjectKey{Name: obj.GetName(), Namespace: ws.Namespace}, obj)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if obj.GetLabels()[LabelWorkspaceUID] != string(uid) {
			return nil // foreign-owned same-named object: not ours to delete
		}
		if obj.GetDeletionTimestamp().IsZero() {
			return b.client.Delete(ctx, obj)
		}
		return nil
	}

	if err := del("Pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: PodName(uid)}}); err != nil {
		return err
	}
	if err := del("Service", &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ServiceName(uid)}}); err != nil {
		return err
	}
	if err := del("Secret", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName(uid)}}); err != nil {
		return err
	}
	if err := del("NetworkPolicy", &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: NetPolName(uid)}}); err != nil {
		return err
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err := b.client.Get(ctx, client.ObjectKey{Name: pvcNameFor(ws), Namespace: ws.Namespace}, pvc)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case pvc.Labels[LabelDataRetained] == "true":
		// A platform-retained volume is never deleted here regardless of
		// policy or ownership labels: persistent data is destroyed only
		// through the explicit purge path.
	case pvc.Labels[LabelWorkspaceUID] != string(uid):
		// foreign PVC squatting on the name — leave it alone
	case ws.Spec.DataPolicy == workspacesv1alpha1.DataPolicyRetain:
		// Retention contract: no ownerReference was ever set, so it survives
		// workspace deletion. Mark it for the retained-inventory flow.
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Labels[LabelDataRetained] = "true"
		if err := b.client.Patch(ctx, pvc, patch); err != nil {
			return err
		}
	default:
		if pvc.DeletionTimestamp.IsZero() {
			return b.client.Delete(ctx, pvc)
		}
	}
	return nil
}

// --- children ---------------------------------------------------------------

func (b *Backend) ensureSecret(ctx context.Context, ws *workspacesv1alpha1.Workspace) (*corev1.Secret, error) {
	uid := ws.UID
	sec := &corev1.Secret{}
	err := b.client.Get(ctx, client.ObjectKey{Name: SecretName(uid), Namespace: ws.Namespace}, sec)
	if err == nil {
		if err := checkOwned("Secret", sec, uid); err != nil {
			return nil, err
		}
		return sec, nil // never rotate an existing credential
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	data, err := generateCredentials(uid)
	if err != nil {
		return nil, fmt.Errorf("generate runtime credential: %w", err)
	}
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(uid),
			Namespace: ws.Namespace,
			Labels:    labels(ws),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := controllerutil.SetControllerReference(ws, sec, b.client.Scheme()); err != nil {
		return nil, err
	}
	if err := b.client.Create(ctx, sec); err != nil {
		return nil, err
	}
	return sec, nil
}

// generateCredentials creates the per-workspace runtime credential: a random
// password plus a self-signed TLS cert for the :8443 endpoint. Values only
// ever live in the Secret; callers must not log them.
func generateCredentials(uid CRUID) (map[string][]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: PodName(uid)},
		DNSNames:     []string{PodName(uid), ServiceName(uid), "localhost"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	// Secret files are newline-terminated: kasmvncpasswd reads its password
	// via getpass(), which requires a '\n' before EOF.
	return map[string][]byte{
		"username": []byte("kasm_user\n"),
		"password": []byte(base64.RawURLEncoding.EncodeToString(raw) + "\n"),
		"tls.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"tls.key":  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func (b *Backend) ensurePVC(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (*corev1.PersistentVolumeClaim, error) {
	uid := ws.UID
	if ref := ws.Annotations[AnnotationRetainedPVC]; ref != "" {
		// Attach path (design §5): mount the retained PVC by reference —
		// never create a fresh volume in its place. The claim must carry
		// the retained marker and be claimed for this workspace UID by the
		// applier; anything else is a transient state the next reconcile
		// resolves.
		pvc := &corev1.PersistentVolumeClaim{}
		err := b.client.Get(ctx, client.ObjectKey{Name: ref, Namespace: ws.Namespace}, pvc)
		if err != nil {
			return nil, fmt.Errorf("linux backend: retained PVC %q: %w", ref, err)
		}
		if pvc.Labels[LabelDataRetained] != "true" {
			return nil, &conflictError{kind: "PersistentVolumeClaim", name: ref}
		}
		if pvc.Labels[LabelWorkspaceUID] != string(uid) {
			return nil, fmt.Errorf("linux backend: retained PVC %q not yet claimed by workspace %s", ref, string(uid))
		}
		return pvc, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := b.client.Get(ctx, client.ObjectKey{Name: PVCName(uid), Namespace: ws.Namespace}, pvc)
	if err == nil {
		if err := checkOwned("PersistentVolumeClaim", pvc, uid); err != nil {
			return nil, err
		}
		return pvc, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	l := labels(ws)
	l[LabelDataRole] = string(workspacesv1alpha1.DataRoleHome)
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PVCName(uid),
			Namespace: ws.Namespace,
			Labels:    l,
			// Retention contract: deliberately NO ownerReference. The PVC must
			// survive Workspace deletion; status.dataRefs is the bookkeeping.
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: tpl.Spec.Resources.Storage,
				},
			},
		},
	}
	if sc := tpl.Annotations[AnnotationStorageClass]; sc != "" {
		pvc.Spec.StorageClassName = &sc
	}
	if err := b.client.Create(ctx, pvc); err != nil {
		return nil, err
	}
	return pvc, nil
}

func (b *Backend) ensurePod(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate, appArmor *corev1.AppArmorProfile) error {
	uid := ws.UID
	pod := &corev1.Pod{}
	err := b.client.Get(ctx, client.ObjectKey{Name: PodName(uid), Namespace: ws.Namespace}, pod)
	switch {
	case apierrors.IsNotFound(err):
		pod = buildPod(ws, tpl, appArmor, b.opts)
		if err := controllerutil.SetControllerReference(ws, pod, b.client.Scheme()); err != nil {
			return err
		}
		return b.client.Create(ctx, pod)
	case err != nil:
		return err
	}
	if err := checkOwned("Pod", pod, uid); err != nil {
		return err
	}
	// Pod belongs to a fenced-off generation (spec.runtimeGeneration moved on
	// after a stop/start): replace it so the incarnation identity matches the
	// current generation.
	gen := fmt.Sprintf("%d", ws.Spec.RuntimeGeneration)
	if pod.Labels[LabelRuntimeGeneration] != gen && pod.DeletionTimestamp.IsZero() {
		return b.client.Delete(ctx, pod)
	}
	return nil
}

// ephemeralHomeSize bounds the Ephemeral-policy home emptyDir by the
// template's declared storage size — the same quantity a Retain policy
// would provision as a PVC. A non-positive quantity gets a safe default
// so a degenerate spec still yields a bounded volume (SEC-11).
func ephemeralHomeSize(tpl *workspacesv1alpha1.WorkspaceTemplate) resource.Quantity {
	q := tpl.Spec.Resources.Storage.DeepCopy()
	if q.Sign() <= 0 {
		q = resource.MustParse("1Gi")
	}
	return q
}

var (
	// tmpSizeLimit bounds the /tmp scratch emptyDir (session downloads,
	// installer staging) inside the pod's ephemeral-storage budget.
	tmpSizeLimit = resource.MustParse("1Gi")
	// rootfsAllowance covers the container's writable layer plus pod logs
	// on top of the mounted emptyDir budgets.
	rootfsAllowance = resource.MustParse("1Gi")
)

// ephemeralStorageBudget is the container's ephemeral-storage
// request/limit: every disk-backed volume the pod can fill plus the
// writable-rootfs/log allowance. A Retain home is a PVC — it is not
// counted in pod ephemeral storage.
func ephemeralStorageBudget(ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) resource.Quantity {
	budget := tmpSizeLimit.DeepCopy()
	budget.Add(rootfsAllowance)
	if ws.Spec.DataPolicy == workspacesv1alpha1.DataPolicyEphemeral {
		budget.Add(ephemeralHomeSize(tpl))
	}
	return budget
}

func shmSizeLimit(tpl *workspacesv1alpha1.WorkspaceTemplate) resource.Quantity {
	// /dev/shm is a Memory emptyDir: its sizeLimit is counted against the
	// pod's memory cgroup. Bound it at memory/4, clamped to [64Mi, 512Mi].
	q := tpl.Spec.Resources.Memory.DeepCopy()
	q.Set(q.Value() / 4)
	min := resource.MustParse("64Mi")
	max := resource.MustParse("512Mi")
	if q.Cmp(min) < 0 {
		q = min
	}
	if q.Cmp(max) > 0 {
		q = max
	}
	return q
}

// resolveAppArmorProfile maps the optional apparmor-profile template
// annotation to a SecurityContext.AppArmorProfile (K8s 1.30+). Accepted
// values: unset or "runtime/default" (the containerd default profile, which
// is what the field defaults to anyway — recorded explicitly so the pod
// object is self-describing), and "localhost/<name>" for a profile the node
// has already loaded (the D-NODE browser-sandbox profile). Everything else —
// "unconfined", "docker/default", an empty localhost name — is a policy
// rejection: Unconfined is never selectable from a template.
func resolveAppArmorProfile(tpl *workspacesv1alpha1.WorkspaceTemplate) (*corev1.AppArmorProfile, error) {
	v := tpl.Annotations[AnnotationAppArmorProfile]
	switch {
	case v == "" || v == "runtime/default":
		return &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}, nil
	case strings.HasPrefix(v, "localhost/") && validAppArmorProfileName(v[len("localhost/"):]):
		name := v[len("localhost/"):]
		return &corev1.AppArmorProfile{
			Type:             corev1.AppArmorProfileTypeLocalhost,
			LocalhostProfile: &name,
		}, nil
	default:
		return nil, &templateRejectedError{reason: fmt.Sprintf(
			"annotation %s=%q: only \"localhost/<name>\" or \"runtime/default\" are accepted",
			AnnotationAppArmorProfile, v)}
	}
}

// validAppArmorProfileName: the node-loaded profile reference must be a
// bare name — non-empty, no path traversal, no whitespace.
func validAppArmorProfileName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/ \t\n") {
		return false
	}
	return true
}

func buildPod(ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate, appArmor *corev1.AppArmorProfile, opts Options) *corev1.Pod {
	if opts.AppArmorNotRequired && appArmor != nil && appArmor.Type == corev1.AppArmorProfileTypeRuntimeDefault {
		appArmor = nil
	}
	uid := ws.UID
	l := labels(ws)
	l[LabelRuntimeGeneration] = fmt.Sprintf("%d", ws.Spec.RuntimeGeneration)

	seccomp := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	if p := tpl.Annotations[AnnotationSeccompProfile]; len(p) > len("localhost/") &&
		p[:len("localhost/")] == "localhost/" {
		name := p[len("localhost/"):]
		seccomp = &corev1.SeccompProfile{
			Type:             corev1.SeccompProfileTypeLocalhost,
			LocalhostProfile: &name,
		}
	}

	home := corev1.Volume{
		Name:         "home",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	if ws.Spec.DataPolicy == workspacesv1alpha1.DataPolicyRetain {
		home.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcNameFor(ws),
			},
		}
	} else {
		// SEC-11: the Ephemeral home is node-disk backed — bound it or a
		// workspace user can fill the node's ephemeral storage.
		homeSize := ephemeralHomeSize(tpl)
		home.VolumeSource.EmptyDir.SizeLimit = &homeSize
	}
	shm := shmSizeLimit(tpl)
	rt := resource.MustParse("32Mi")
	tmp := tmpSizeLimit.DeepCopy()
	eph := ephemeralStorageBudget(ws, tpl)
	secretMode := int32(0o440)

	ctr := corev1.Container{
		Name:  "desktop",
		Image: tpl.Spec.Linux.Image,
		Ports: []corev1.ContainerPort{{
			Name:          "streaming",
			ContainerPort: streamingPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              tpl.Spec.Resources.CPU,
				corev1.ResourceMemory:           tpl.Spec.Resources.Memory,
				corev1.ResourceEphemeralStorage: eph,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              tpl.Spec.Resources.CPU,
				corev1.ResourceMemory:           tpl.Spec.Resources.Memory,
				corev1.ResourceEphemeralStorage: eph,
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"/opt/tcdi/healthcheck.sh"}},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
			FailureThreshold:    12,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			RunAsNonRoot:             ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           seccomp,
			AppArmorProfile:          appArmor,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "secrets", MountPath: secretMountDir, ReadOnly: true},
			{Name: "rt", MountPath: runtimeDir},
			{Name: "dshm", MountPath: shmDir},
			{Name: "home", MountPath: homeDir},
			{Name: "tmp", MountPath: "/tmp"},
		},
	}
	if len(tpl.Spec.Linux.Command) > 0 {
		ctr.Command = tpl.Spec.Linux.Command
	}

	kasm := tpl.Spec.Linux.Adapter == workspacesv1alpha1.AdapterKasm
	if kasm {
		// Approach A (docs/kasm-images.md): the UNMODIFIED kasmweb/* image's
		// own startup is bypassed — the adapter scripts (delivered into the
		// tcdi-adapter emptyDir by the initContainer below) drive
		// kasmvncserver directly. spec.linux.command is already forbidden
		// with adapter=kasm by CEL; the entrypoint override wins regardless.
		ctr.Command = []string{adapterEntrypoint}
		// The kasm images bake VNC_PW/VNC_VIEW_ONLY_PW defaults into their
		// image env (Kasm's own startup consumes them); the adapter never
		// reads them — neutralize the defaults for hygiene. HOME points at
		// the mounted workspace volume (the image's native home is
		// /home/kasm-user).
		ctr.Env = append(ctr.Env,
			corev1.EnvVar{Name: "HOME", Value: homeDir},
			corev1.EnvVar{Name: "VNC_PW", Value: ""},
			corev1.EnvVar{Name: "VNC_VIEW_ONLY_PW", Value: ""},
		)
		if tpl.Spec.Linux.SessionCmd != "" {
			ctr.Env = append(ctr.Env,
				corev1.EnvVar{Name: "TCDI_SESSION_CMD", Value: tpl.Spec.Linux.SessionCmd})
		}
		ctr.VolumeMounts = append(ctr.VolumeMounts,
			corev1.VolumeMount{Name: adapterVolName, MountPath: adapterDir, ReadOnly: true})
		// KASM-1: shadow every Chromium-family wrapper with the read-only
		// sandbox-preserving shim (see AdapterBrowserWrappers).
		for _, wp := range AdapterBrowserWrappers {
			ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{
				Name:      adapterVolName,
				MountPath: wp,
				SubPath:   adapterShimSubPath,
				ReadOnly:  true,
			})
		}
		// KASM-8: managed Chromium policy over each engine's policy dir.
		for _, pp := range AdapterPolicyTargets {
			ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{
				Name:      adapterVolName,
				MountPath: pp,
				SubPath:   adapterPolicySubPath,
				ReadOnly:  true,
			})
		}
		// KASM-6/7: the kasmweb rootfs ships world-writable policy files and
		// a uid-1000-writable dir inside the served web root
		// (/usr/share/kasmvnc/www/Downloads → symlink-following reads). All
		// legitimate writes already land on mounts (home volume, /run/tcdi,
		// /tmp, /dev/shm, /opt/tcdi), so the rest of the rootfs is read-only.
		ctr.SecurityContext.ReadOnlyRootFilesystem = ptr(true)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodName(uid),
			Namespace: ws.Namespace,
			Labels:    l,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			ServiceAccountName:            RuntimeServiceAccount,
			AutomountServiceAccountToken:  ptr(false),
			EnableServiceLinks:            ptr(false),
			TerminationGracePeriodSeconds: ptr(int64(30)),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				RunAsUser:      ptr(int64(1000)),
				RunAsGroup:     ptr(int64(1000)),
				FSGroup:        ptr(int64(1000)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{ctr},
			Volumes: []corev1.Volume{
				{Name: "secrets", VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  SecretName(uid),
						DefaultMode: &secretMode,
					}}},
				{Name: "rt", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: &rt,
					}}},
				{Name: "dshm", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: &shm,
					}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: &tmp,
					}}},
				home,
			},
		},
	}
	if kasm {
		// The copier's explicit RuntimeDefault follows the same setting as
		// the desktop container: a host without AppArmor refuses it too.
		var initAppArmor *corev1.AppArmorProfile
		if !opts.AppArmorNotRequired {
			initAppArmor = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}
		}
		// The adapter initContainer copies the adapter scripts into the
		// shared emptyDir — the only mutation the foreign image gets. Its
		// confinement mirrors the desktop's minus the browser's Localhost
		// profiles (a static copier needs only RuntimeDefault).
		pod.Spec.InitContainers = []corev1.Container{{
			Name:    adapterInitName,
			Image:   opts.KasmAdapterImage,
			Command: []string{adapterInstaller},
			// KASM-3: requests=limits so ResourceQuota-governed namespaces
			// admit the pod (quota admission checks initContainers too).
			// The copier is a ~5 MB static binary — the budget is tight on
			// purpose.
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("10m"),
					corev1.ResourceMemory:           resource.MustParse("32Mi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("16Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("10m"),
					corev1.ResourceMemory:           resource.MustParse("32Mi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("16Mi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr(false),
				RunAsNonRoot:             ptr(true),
				// The copier only writes the mounted adapter volume.
				ReadOnlyRootFilesystem: ptr(true),
				Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:         &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				AppArmorProfile:        initAppArmor,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: adapterVolName, MountPath: adapterDir},
			},
		}}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         adapterVolName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	// Placement, per field: spec.placement → legacy node-selector
	// annotation (nodeSelector only; deprecated, one release of overlap —
	// D24) → operator default. A set field replaces the lower layers
	// outright; nothing is merged.
	nodeSelector := opts.DefaultPlacement.NodeSelector
	tolerations := opts.DefaultPlacement.Tolerations
	runtimeClass := opts.DefaultPlacement.RuntimeClassName
	if raw := tpl.Annotations[AnnotationNodeSelector]; raw != "" {
		var sel map[string]string
		if json.Unmarshal([]byte(raw), &sel) == nil && len(sel) > 0 {
			nodeSelector = sel
		}
	}
	if p := tpl.Spec.Placement; p != nil {
		if len(p.NodeSelector) > 0 {
			nodeSelector = p.NodeSelector
		}
		if len(p.Tolerations) > 0 {
			tolerations = p.Tolerations
		}
		if p.RuntimeClassName != nil {
			runtimeClass = p.RuntimeClassName
		}
	}
	pod.Spec.NodeSelector = nodeSelector
	pod.Spec.Tolerations = tolerations
	pod.Spec.RuntimeClassName = runtimeClass

	// hostUsers: template field → operator default; nil leaves the pod
	// field unset (D26 — opt-in until the R1 spike lands).
	hostUsers := opts.DefaultHostUsers
	if h := tpl.Spec.Linux.HostUsers; h != nil {
		hostUsers = h
	}
	pod.Spec.HostUsers = hostUsers
	return pod
}

func (b *Backend) ensureService(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	uid := ws.UID
	svc := &corev1.Service{}
	err := b.client.Get(ctx, client.ObjectKey{Name: ServiceName(uid), Namespace: ws.Namespace}, svc)
	switch {
	case apierrors.IsNotFound(err):
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ServiceName(uid),
				Namespace: ws.Namespace,
				Labels:    labels(ws),
			},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Selector: map[string]string{
					LabelWorkspaceUID: string(uid),
				},
				Ports: []corev1.ServicePort{{
					Name:       "streaming",
					Protocol:   corev1.ProtocolTCP,
					Port:       streamingPort,
					TargetPort: intstr.FromString("streaming"),
				}},
			},
		}
		if err := controllerutil.SetControllerReference(ws, svc, b.client.Scheme()); err != nil {
			return err
		}
		return b.client.Create(ctx, svc)
	case err != nil:
		return err
	}
	return checkOwned("Service", svc, uid)
}

func (b *Backend) ensureNetPol(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) error {
	uid := ws.UID
	np := &networkingv1.NetworkPolicy{}
	err := b.client.Get(ctx, client.ObjectKey{Name: NetPolName(uid), Namespace: ws.Namespace}, np)
	switch {
	case apierrors.IsNotFound(err):
		np = buildNetPol(ws, tpl, b.opts)
		if err := controllerutil.SetControllerReference(ws, np, b.client.Scheme()); err != nil {
			return err
		}
		return b.client.Create(ctx, np)
	case err != nil:
		return err
	}
	if err := checkOwned("NetworkPolicy", np, uid); err != nil {
		return err
	}
	// Reconcile drift (SEC-02): the policy spec is fully declarative, so a
	// stored spec that differs from the desired one — an older except
	// list, a tampered peer, a profile change — is rewritten in place.
	// Without this, hardened operator options would never reach the
	// workspaces that already exist.
	desired := buildNetPol(ws, tpl, b.opts)
	if apiequality.Semantic.DeepEqual(np.Spec, desired.Spec) {
		return nil
	}
	np.Spec = desired.Spec
	return b.client.Update(ctx, np)
}

func buildNetPol(ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate, opts Options) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	streamPort := intstr.FromInt32(streamingPort)

	// (a) cluster DNS — coredns by pod identity, UDP+TCP 53.
	dns := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"k8s-app": "kube-dns"},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &dnsPort},
			{Protocol: &tcp, Port: &dnsPort},
		},
	}

	var profile []networkingv1.NetworkPolicyEgressRule
	switch tpl.Spec.NetworkProfile {
	case workspacesv1alpha1.NetworkProfileInternetOnly:
		// (c) internet minus the always-on private/reserved excepts and
		// the configured cluster pod/service/node CIDRs (the proven
		// network-isolation fixture policy shape, SEC-02).
		except := egressExcepts(opts)
		profile = append(profile, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: except},
			}},
		})
	case workspacesv1alpha1.NetworkProfileClusterOnly:
		// In-cluster destinations only: any pod in any namespace.
		profile = append(profile, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{PodSelector: &metav1.LabelSelector{}},
				{NamespaceSelector: &metav1.LabelSelector{}},
			},
		})
	}
	// NetworkProfileIsolated: DNS only, no extra rules.

	// Ingress is scoped to gateway pods living in the platform namespace
	// (SEC-29): a pod labeled role=gateway anywhere else must not satisfy
	// the peer. With no configured gateway namespace the policy fails
	// closed to the workspace's own namespace — never "any namespace".
	gatewayNS := opts.GatewayNamespace
	if gatewayNS == "" {
		gatewayNS = ws.Namespace
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NetPolName(ws.UID),
			Namespace: ws.Namespace,
			Labels:    labels(ws),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{LabelWorkspaceUID: string(ws.UID)},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Ingress: only gateway pods may reach the runtime, on the
			// streaming port only (design §6: gateway -> runtime).
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": gatewayNS,
						},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{LabelRole: RoleGateway},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &tcp,
					Port:     &streamPort,
				}},
			}},
			Egress: append([]networkingv1.NetworkPolicyEgressRule{dns}, profile...),
		},
	}
}

// --- observation helpers -----------------------------------------------------

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podReason derives the machine-readable observation reason from pod state:
// scheduling failures, container waits, terminal phases, else
// provisioning/ready.
func podReason(pod *corev1.Pod) string {
	if !pod.DeletionTimestamp.IsZero() {
		return "Terminating"
	}
	switch pod.Status.Phase {
	case corev1.PodFailed:
		return "PodFailed"
	case corev1.PodSucceeded:
		return "PodExited"
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason // ImagePullBackOff, CrashLoopBackOff, CreateContainerConfigError...
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			if c.Reason != "" {
				return c.Reason // e.g. Unschedulable
			}
			return "Unschedulable"
		}
	}
	if podReady(pod) {
		return "Ready"
	}
	if pod.Status.Phase == corev1.PodRunning {
		return "NotReady"
	}
	return "Provisioning"
}

func ptr[T any](v T) *T { return &v }
