// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Unit coverage for the adapter=kasm pod shape (docs/kasm-images.md):
// templates running UNMODIFIED kasmweb/* images get the adapter injected
// via an initContainer writing the adapter scripts into a shared emptyDir
// that the desktop container mounts read-only, plus the adapter env
// contract (TCDI_SESSION_CMD, neutralized VNC_PW/VNC_VIEW_ONLY_PW, HOME).
// Everything else about the pod — port, probe, secret mount, shm, home,
// securityContext — must be identical to a native template.
package linux

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

const testAdapterImage = "ghcr.io/tinyorbitvn/tinycdi-kasm-adapter@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111"

// ensureKasmPod runs Ensure for a kasm template and returns the Pod spec.
func ensureKasmPod(t *testing.T, sessionCmd string, opts Options) *corev1.Pod {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.Linux.Adapter = workspacesv1alpha1.AdapterKasm
	tpl.Spec.Linux.SessionCmd = sessionCmd
	if _, err := New(c, opts).Ensure(context.Background(), ws, tpl); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	return pod
}

func envOf(c *corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestKasmAdapterPodShape(t *testing.T) {
	pod := ensureKasmPod(t, "/usr/bin/chromium-orig --start-maximized",
		Options{KasmAdapterImage: testAdapterImage})

	// InitContainer: operator-pinned image, installs into the shared dir.
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("initContainers = %+v, want exactly 1", pod.Spec.InitContainers)
	}
	init := pod.Spec.InitContainers[0]
	if init.Name != adapterInitName {
		t.Errorf("init container name = %q, want %q", init.Name, adapterInitName)
	}
	if init.Image != testAdapterImage {
		t.Errorf("init image = %q, want %q", init.Image, testAdapterImage)
	}
	if len(init.Command) != 1 || init.Command[0] != adapterInstaller {
		t.Errorf("init command = %v, want [%s]", init.Command, adapterInstaller)
	}
	sc := init.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		sc.AppArmorProfile == nil || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeRuntimeDefault {
		t.Errorf("init securityContext must be the hardened RuntimeDefault baseline, got %+v", sc)
	}
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != adapterVolName ||
		init.VolumeMounts[0].MountPath != adapterDir || init.VolumeMounts[0].ReadOnly {
		t.Errorf("init mounts = %+v, want rw mount of %s at %s", init.VolumeMounts, adapterVolName, adapterDir)
	}

	// Desktop container: adapter entrypoint, env contract, ro mount.
	ctr := pod.Spec.Containers[0]
	if len(ctr.Command) != 1 || ctr.Command[0] != adapterEntrypoint {
		t.Errorf("command = %v, want [%s]", ctr.Command, adapterEntrypoint)
	}
	for k, want := range map[string]string{
		"HOME":             homeDir,
		"VNC_PW":           "",
		"VNC_VIEW_ONLY_PW": "",
		"TCDI_SESSION_CMD": "/usr/bin/chromium-orig --start-maximized",
	} {
		if v, ok := envOf(&ctr, k); !ok || v != want {
			t.Errorf("env %s = %q (present=%v), want %q", k, v, ok, want)
		}
	}
	var mount *corev1.VolumeMount
	for i := range ctr.VolumeMounts {
		if ctr.VolumeMounts[i].Name == adapterVolName {
			mount = &ctr.VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly || mount.MountPath != adapterDir {
		t.Fatalf("adapter mount missing/not read-only at %s: %+v", adapterDir, ctr.VolumeMounts)
	}

	// The shared emptyDir volume exists exactly once.
	found := 0
	for _, v := range pod.Spec.Volumes {
		if v.Name == adapterVolName {
			found++
			if v.EmptyDir == nil {
				t.Errorf("%s volume is not an emptyDir", adapterVolName)
			}
		}
	}
	if found != 1 {
		t.Errorf("volumes must hold exactly one %s emptyDir, got %d", adapterVolName, found)
	}

	// The rest of the contract is untouched: port, probe, secret mount,
	// securityContext, service account posture.
	if ctr.Ports[0].ContainerPort != streamingPort {
		t.Errorf("streaming port changed: %+v", ctr.Ports)
	}
	if ctr.ReadinessProbe == nil || ctr.ReadinessProbe.Exec == nil ||
		ctr.ReadinessProbe.Exec.Command[0] != "/opt/tcdi/healthcheck.sh" {
		t.Errorf("readiness probe changed: %+v", ctr.ReadinessProbe)
	}
	if len(ctr.SecurityContext.Capabilities.Drop) != 1 ||
		ctr.SecurityContext.Capabilities.Drop[0] != "ALL" ||
		!*ctr.SecurityContext.RunAsNonRoot {
		t.Errorf("container securityContext changed: %+v", ctr.SecurityContext)
	}
	if pod.Spec.ServiceAccountName != RuntimeServiceAccount {
		t.Errorf("serviceAccountName = %q, want %q", pod.Spec.ServiceAccountName, RuntimeServiceAccount)
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must stay false")
	}
}

func TestKasmAdapterNoSessionCmd(t *testing.T) {
	pod := ensureKasmPod(t, "", Options{KasmAdapterImage: testAdapterImage})
	ctr := pod.Spec.Containers[0]
	if _, ok := envOf(&ctr, "TCDI_SESSION_CMD"); ok {
		t.Error("empty sessionCmd must not render a TCDI_SESSION_CMD env var")
	}
	// The rest of the adapter deltas still apply.
	if ctr.Command[0] != adapterEntrypoint {
		t.Errorf("command = %v, want [%s]", ctr.Command, adapterEntrypoint)
	}
}

func TestKasmAdapterRejectedWithoutImage(t *testing.T) {
	// The adapter init image is operator configuration, never template
	// input: without --kasm-adapter-image a kasm template is rejected
	// BEFORE any child object exists.
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.Linux.Adapter = workspacesv1alpha1.AdapterKasm
	_, err := New(c, Options{}).Ensure(context.Background(), ws, tpl)
	if err == nil {
		t.Fatal("adapter=kasm without KasmAdapterImage must be rejected")
	}
	if !isTemplateRejected(err) {
		t.Fatalf("want ErrTemplateRejected, got %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err == nil {
		t.Fatal("a rejected template must create NO pod")
	}
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: SecretName(ws.UID), Namespace: ws.Namespace}, sec); err == nil {
		t.Fatal("a rejected template must create NO secret")
	}
}

func TestNoAdapterPodUnchanged(t *testing.T) {
	// Default path: no initContainer, no adapter volume/env, image entrypoint.
	pod := ensurePodSpec(t, nil)
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("no-adapter pod must not have initContainers: %+v", pod.Spec.InitContainers)
	}
	ctr := pod.Spec.Containers[0]
	if len(ctr.Command) != 0 {
		t.Errorf("no-adapter pod must keep the image entrypoint, command=%v", ctr.Command)
	}
	if len(ctr.Env) != 0 {
		t.Errorf("no-adapter pod must not set env: %v", ctr.Env)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == adapterVolName {
			t.Error("no-adapter pod must not carry the adapter volume")
		}
	}
}

func isTemplateRejected(err error) bool {
	return err != nil && errors.Is(err, ErrTemplateRejected)
}
