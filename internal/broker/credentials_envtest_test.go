package broker_test

// envtest coverage for the credential-binding defect: the broker must
// resolve the runtime credential Secret and the upstream Service from the
// Workspace CR's metadata.uid — the identity the operator's linux backend
// names child objects from — never from the platform workspace id
// (ws_…). On a real cluster the two identities always differ; a Secret
// lookup keyed by the platform id can only miss.

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// TestResolveTarget_CRUIDChildren runs the full issue→redeem→resolve path
// against a real apiserver: the Workspace CR carries a platform id that is
// NOT its metadata.uid, the operator-created Secret and Service are named
// from the CR UID, and ResolveTarget must return that Secret's credentials
// plus the Service-derived upstream URL.
func TestResolveTarget_CRUIDChildren(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: envtestAssets(t),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	kc, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "w5cred-"}}
	if err := kc.Create(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	// The CR the applier would have created: the workspace-uid label holds
	// the platform id; metadata.uid is apiserver-assigned and never equal.
	wsUID := provisioning.PlatformID("ws_cred01")
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-cred01",
			Namespace: ns.Name,
			Labels: map[string]string{
				provisioning.LabelWorkspaceUID: string(wsUID),
				provisioning.LabelTenant:       "tenant-a",
			},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "tpl"},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://idp.example", Subject: "alice"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	if err := kc.Create(ctx, ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	crUID := ws.UID
	if string(crUID) == string(wsUID) {
		t.Fatalf("test requires distinct identities, got %q", crUID)
	}

	// Operator-created children, named from the CR UID (linux backend).
	tlsCA := []byte("-----BEGIN CERTIFICATE-----\ncrud-cert\n-----END CERTIFICATE-----\n")
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linux.SecretName(crUID),
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"username": []byte("kasm_user\n"),
			"password": []byte("s3cret-pw\n"),
			"tls.crt":  tlsCA,
		},
	}
	if err := kc.Create(ctx, sec); err != nil {
		t.Fatalf("runtime secret: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linux.ServiceName(crUID),
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{
				Name:       "streaming",
				Port:       8443,
				TargetPort: intstr.FromInt32(8443),
			}},
		},
	}
	if err := kc.Create(ctx, svc); err != nil {
		t.Fatalf("runtime service: %v", err)
	}

	// What the operator reports once Ensure converged.
	ws.Status = workspacesv1alpha1.WorkspaceStatus{
		Phase:                     workspacesv1alpha1.WorkspacePhaseReady,
		ObservedRuntimeGeneration: 1,
		RuntimeUID:                "rt-uid-1",
		ServiceRef:                &workspacesv1alpha1.ServiceReference{Name: svc.Name, Port: 8443},
	}
	if err := kc.Status().Update(ctx, ws); err != nil {
		t.Fatalf("status update: %v", err)
	}

	// Production wiring: informer binding source + namespaced Secret reader.
	resync := time.Second
	kcache, err := crcache.New(restCfg, crcache.Options{
		Scheme:            scheme,
		SyncPeriod:        &resync,
		DefaultNamespaces: map[string]crcache.Config{ns.Name: {}},
	})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	src, err := broker.NewK8sBindingSource(ctx, kcache)
	if err != nil {
		t.Fatalf("binding source: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = kcache.Start(cctx) }()
	if !kcache.WaitForCacheSync(cctx) {
		t.Fatal("cache did not sync")
	}
	src.MarkSynced()

	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "https://idp.example|alice", string(wsUID))
	b := broker.New(db, src, broker.WithCredentialSource(
		broker.NewK8sCredentialSource(kc, provisioning.TenantNamespaces{"tenant-a": ns.Name})))

	// The informer must project the CR UID alongside the platform id.
	var binding broker.RuntimeBinding
	deadline := time.Now().Add(10 * time.Second)
	for {
		binding, err = src.CurrentBinding(ctx, wsUID)
		if err == nil && binding.CRUID == crUID && binding.Phase == broker.PhaseReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("binding never projected CRUID %q: %+v (err=%v)", crUID, binding, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if binding.CRUID == types.UID(wsUID) {
		t.Fatalf("binding CRUID %q equals platform id — wrong identity", binding.CRUID)
	}

	alice := api.Principal{Issuer: "https://idp.example", Subject: "alice", TenantID: "tenant-a"}
	gw := broker.GatewayIdentity{ID: "gw-1", Audience: broker.DefaultGatewayAudience}
	tk, err := b.IssueTicket(ctx, alice, wsUID, false, "")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	lease, err := b.RedeemTicket(ctx, gw, tk.Token)
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	target, err := b.ResolveTarget(ctx, gw, lease.ID)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}

	if want := "https://" + svc.Name + "." + ns.Name + ".svc:8443"; target.UpstreamURL != want {
		t.Fatalf("UpstreamURL = %q, want %q (CR-UID-derived Service)", target.UpstreamURL, want)
	}
	if target.TLSServerName != svc.Name {
		t.Fatalf("TLSServerName = %q, want %q", target.TLSServerName, svc.Name)
	}
	if target.Username != "kasm_user" || target.Password != "s3cret-pw" {
		t.Fatalf("target credentials = %q/%q, want the operator Secret's values",
			target.Username, target.Password)
	}
	if !bytes.Equal(target.TLSCA, tlsCA) {
		t.Fatal("target TLSCA does not match the runtime Secret's tls.crt")
	}
}
