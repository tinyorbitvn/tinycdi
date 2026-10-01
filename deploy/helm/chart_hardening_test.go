// Security-hardening assertions (CHTR-1/2/3/4/7 + the
// oidc.requiredGroups login gate).
//
// Helpers are shared with chart_test.go (same package). New tests only —
// chart_test.go holds the earlier assertions.
package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CHTR-1 — the verifier must read the REAL loaded-profiles list.
// ---------------------------------------------------------------------------

// TestHardeningVerifierLeastPrivilege: apparmorfs serves
// /sys/kernel/security/apparmor/profiles only to uid 0 regardless of its
// 0444 mode, so the verifier runs as uid 0 — but with every capability
// dropped, no privilege escalation, a read-only rootfs and read-only host
// mounts (the least privilege that can still verify on a real kernel).
// The securityfs mount is /sys/kernel/security itself (a separate
// filesystem — a plain /sys bind does not carry it into the container).
func TestHardeningVerifierLeastPrivilege(t *testing.T) {
	docs := render(t, "node-profiles-values.yaml")
	ds := daemonSet(docs, "node-profiles")
	if ds == nil {
		t.Fatal("no node-profiles DaemonSet rendered")
	}
	podSpec := dsPodSpec(ds)
	containers := toSlice(podSpec["containers"])
	if len(containers) != 1 {
		t.Fatalf("expected exactly 1 verifier container, got %d", len(containers))
	}
	c, _ := containers[0].(map[string]any)
	vsc, _ := c["securityContext"].(map[string]any)
	if vsc["runAsUser"] != 0 && vsc["runAsUser"] != float64(0) {
		t.Errorf("verifier must run as uid 0 (only root can read the loaded-profiles list), got %v", vsc["runAsUser"])
	}
	if vsc["runAsNonRoot"] == true {
		t.Error("verifier cannot be runAsNonRoot — it must be uid 0")
	}
	if vsc["allowPrivilegeEscalation"] != false {
		t.Error("verifier must set allowPrivilegeEscalation: false")
	}
	if vsc["readOnlyRootFilesystem"] != true {
		t.Error("verifier must keep a read-only rootfs")
	}
	caps, _ := vsc["capabilities"].(map[string]any)
	dropAll := false
	for _, dr := range toSlice(caps["drop"]) {
		if dr == "ALL" {
			dropAll = true
		}
	}
	if !dropAll {
		t.Error("verifier must drop ALL capabilities")
	}
	if sp, _ := vsc["seccompProfile"].(map[string]any); sp["type"] != "RuntimeDefault" {
		t.Error("verifier must keep seccompProfile RuntimeDefault")
	}
	// no capability may be added, and every hostPath mount is read-only.
	if len(toSlice(caps["add"])) > 0 {
		t.Errorf("verifier must not add capabilities, got %v", caps["add"])
	}
	securityfsMounted := false
	for _, vm := range toSlice(c["volumeMounts"]) {
		vmm, _ := vm.(map[string]any)
		if vmm["readOnly"] != true {
			name, _ := vmm["name"].(string)
			if strings.HasPrefix(name, "host-") || name == "profiles" {
				t.Errorf("verifier mount %q must be readOnly", name)
			}
		}
		if vmm["name"] == "host-securityfs" {
			securityfsMounted = true
		}
	}
	if !securityfsMounted {
		t.Error("verifier must mount the host securityfs (host-securityfs volume)")
	}
	// the volume must be the securityfs root, not the whole /sys tree.
	for _, v := range toSlice(podSpec["volumes"]) {
		vm, _ := v.(map[string]any)
		hp, _ := vm["hostPath"].(map[string]any)
		if hp == nil {
			continue
		}
		if hp["path"] == "/sys" {
			t.Error("must not bind-mount all of /sys — mount /sys/kernel/security only")
		}
		if vm["name"] == "host-securityfs" && hp["path"] != "/sys/kernel/security" {
			t.Errorf("host-securityfs must mount /sys/kernel/security, got %v", hp["path"])
		}
	}
	// env must point verify.sh at the mounted securityfs, not /sys.
	for _, e := range toSlice(c["env"]) {
		em, _ := e.(map[string]any)
		if em["name"] == "NODE_PROFILES_AA_SYSFS" && em["value"] != "/host-securityfs/apparmor/profiles" {
			t.Errorf("NODE_PROFILES_AA_SYSFS = %v, want /host-securityfs/apparmor/profiles", em["value"])
		}
	}
}

// TestHardeningVerifierRealSysfsProof execs the shipped verify.sh inside
// the pinned alpine image against the HOST's real securityfs, bind-mounted
// read-only. It proves uid 0 + drop-ALL reads the loaded list (install
// mode Ready; remove mode reports "still loaded"), that uid 65534 gets
// EACCES and fails the probe in BOTH modes (never a false clean), and
// that absent profiles fail install / pass remove honestly.
// Docker-only; skipped when docker or host apparmorfs is unavailable.
func TestHardeningVerifierRealSysfsProof(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker — skipping the real-securityfs verifier proof")
	}
	if _, err := os.Stat("/sys/kernel/security/apparmor/profiles"); err != nil {
		t.Skipf("host has no apparmorfs profiles file: %v", err)
	}
	root := repoRoot(t)
	filesDir := filepath.Join(root, "deploy", "helm", "tinycdi", "files", "node-profiles")
	proofDir := filepath.Join(root, "deploy", "helm", "proof")

	stage := t.TempDir()
	for _, sub := range []string{"installer", "profiles-src", "seccomp", "seccomp-empty"} {
		if err := os.MkdirAll(filepath.Join(stage, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copies := [][2]string{
		{"installer/verify.sh", "installer/verify.sh"},
		{"seccomp/chromium-userns.json", "profiles-src/chromium-userns.json"},
		{"seccomp/chromium-userns.json", "seccomp/chromium-userns.json"},
	}
	for _, cp := range copies {
		b, err := os.ReadFile(filepath.Join(filesDir, cp[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, cp[1]), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	img := "alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507"
	secMount := []string{"-v", filepath.Join(stage, "seccomp") + ":/seccomp:ro"}
	emptyMount := []string{"-v", filepath.Join(stage, "seccomp-empty") + ":/seccomp:ro"}
	run := func(name string, wantOK bool, extra ...string) {
		args := []string{"run", "--rm", "--network", "none", "--name", "tcdi-chart-h-" + name,
			"-v", filepath.Join(stage, "installer") + ":/installer:ro",
			"-v", filepath.Join(stage, "profiles-src") + ":/profiles-src:ro",
			"-v", proofDir + ":/proof:ro",
			"-v", "/sys/kernel/security:/host-securityfs:ro",
		}
		args = append(args, extra...)
		args = append(args, img, "sh", "/proof/node-profiles/run-real-sysfs.sh")
		out, err := exec.Command("docker", args...).CombinedOutput()
		t.Logf("%s err=%v out=%s", name, err, out)
		if wantOK && err != nil {
			t.Errorf("%s: expected verify.sh rc=0, got err=%v out=%s", name, err, out)
		}
		if !wantOK && err == nil {
			t.Errorf("%s: expected verify.sh to fail, it passed: %s", name, out)
		}
	}

	// Pre-flight: the proof needs a host profile to check — docker-default
	// is loaded whenever the docker daemon runs. Skip when absent (e.g.
	// rootless/no-apparmor hosts).
	preOut, preErr := exec.Command("docker", "run", "--rm", "--network", "none",
		"--cap-drop", "ALL",
		"-v", "/sys/kernel/security:/host-securityfs:ro",
		img, "sh", "-c", "grep -q '^docker-default ' /host-securityfs/apparmor/profiles").CombinedOutput()
	if preErr != nil {
		t.Skipf("host apparmorfs lacks docker-default (or unreadable): %v %s", preErr, preOut)
	}

	// install mode: uid 0 + drop ALL verifies the really-loaded profile.
	run("install-uid0", true, append([]string{"--user", "0", "--cap-drop", "ALL",
		"-e", "AA_NAME=docker-default"}, secMount...)...)
	// uid 65534 gets EACCES on the 0444 profiles file — the probe must FAIL
	// (this is exactly why the shipped verifier runs as uid 0).
	run("install-uid65534", false, append([]string{"--user", "65534",
		"-e", "AA_NAME=docker-default"}, secMount...)...)
	// a profile that is NOT loaded must fail even for uid 0.
	run("install-absent", false, append([]string{"--user", "0", "--cap-drop", "ALL",
		"-e", "AA_NAME=tinycdi-browser"}, secMount...)...)
	// remove mode: the loaded profile must be reported still-loaded
	// (the old code read EACCES as "absent" and reported a false clean).
	run("remove-uid0-loaded", false, append([]string{"--user", "0", "--cap-drop", "ALL",
		"-e", "NODE_PROFILES_MODE=remove", "-e", "AA_NAME=docker-default"}, emptyMount...)...)
	run("remove-uid65534-loaded", false, append([]string{"--user", "65534",
		"-e", "NODE_PROFILES_MODE=remove", "-e", "AA_NAME=docker-default"}, emptyMount...)...)
	// remove mode honestly reports clean when nothing is left.
	run("remove-uid0-absent", true, append([]string{"--user", "0", "--cap-drop", "ALL",
		"-e", "NODE_PROFILES_MODE=remove", "-e", "AA_NAME=tinycdi-browser"}, emptyMount...)...)
}

// ---------------------------------------------------------------------------
// CHTR-2 — installer namespace guard + managedEnforce gate.
// ---------------------------------------------------------------------------

// TestHardeningInstallerNamespaceGuard: the installer namespace must be
// dedicated — the render refuses the release namespace, any managed
// namespace, kube-* and default (CHTR-2).
func TestHardeningInstallerNamespaceGuard(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "node-profiles-values.yaml")
	for _, bad := range []string{"tcdi-system", "tinycdi-tenant-a", "kube-system", "kube-public", "kube-node-lease", "kube-foo", "default"} {
		out := renderErrArgs(t, "-f", vf, "--set", "nodeProfiles.install.namespace="+bad)
		if !strings.Contains(out, "nodeProfiles.install.namespace") {
			t.Errorf("namespace %q must fail the render, got: %s", bad, out)
		}
	}
	// a dedicated custom namespace renders.
	renderArgs(t, "-f", vf, "--set", "nodeProfiles.install.namespace=custom-np")
}

// TestHardeningManagedEnforcePrivilegedGated: podSecurity.managedEnforce=
// privileged reopens operator→privileged-pod — dev only (CHTR-2).
func TestHardeningManagedEnforcePrivilegedGated(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "podSecurity.managedEnforce=privileged")
	if !strings.Contains(out, "managedEnforce") {
		t.Errorf("managedEnforce=privileged must fail mentioning managedEnforce, got: %s", out)
	}
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "dev.enabled=true", "--set", "podSecurity.managedEnforce=privileged")
	found := false
	for _, d := range selectDocs(docs, "Namespace") {
		m, _ := d["metadata"].(map[string]any)
		lbls, _ := m["labels"].(map[string]any)
		if lbls["pod-security.kubernetes.io/enforce"] == "privileged" {
			found = true
		}
	}
	if !found {
		t.Error("dev.enabled + managedEnforce=privileged should render privileged labels")
	}
	// baseline is never gated.
	renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "podSecurity.managedEnforce=baseline")
}

// ---------------------------------------------------------------------------
// CHTR-3 — dev-gate bypasses.
// ---------------------------------------------------------------------------

// TestHardeningDevGateBypasses: every surface that weakens the hardened
// defaults fails the render unless dev.enabled (CHTR-3).
func TestHardeningDevGateBypasses(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	cases := []struct {
		name string
		sets []string
	}{
		{"operator dev-allow-no-broker flag", []string{"operator.extraArgs[0]=--dev-allow-no-broker"}},
		{"operator disable builtin egress excepts", []string{"operator.extraArgs[0]=--disable-builtin-egress-excepts"}},
		{"operator metrics re-enabled", []string{"operator.extraArgs[0]=--metrics-bind-address=:8443"}},
		{"operator metrics-secure flag", []string{"operator.extraArgs[0]=--metrics-secure=false"}},
		{"backend dev-insecure-db flag", []string{"backend.extraArgs[0]=--dev-insecure-db"}},
		{"backend required-groups override", []string{"backend.extraArgs[0]=--required-groups=other"}},
		{"backend metrics-listen bypass", []string{"backend.extraArgs[0]=--metrics-listen=:9090"}},
		{"backend split-mode broker-url bypass", []string{"backend.extraArgs[0]=--broker-url=https://broker.example.net:9443"}},
		{"capabilities.drop replaced", []string{"backend.securityContext.capabilities.drop[0]=NET_RAW"}},
		{"capabilities.add", []string{"backend.securityContext.capabilities.add[0]=SYS_ADMIN"}},
		{"appArmorProfile Unconfined", []string{"backend.securityContext.appArmorProfile.type=Unconfined"}},
		{"seLinuxOptions", []string{"backend.securityContext.seLinuxOptions.type=spc_t"}},
		{"fsGroup 0", []string{"backend.podSecurityContext.fsGroup=0"}},
		{"supplementalGroups 0", []string{"backend.podSecurityContext.supplementalGroups[0]=0"}},
		{"hostPath extraVolume", []string{"backend.extraVolumes[0].name=host", "backend.extraVolumes[0].hostPath.path=/"}},
		{"db tls disable", []string{"database.tls.mode=disable"}},
		{"db tls allow", []string{"database.tls.mode=allow"}},
		{"db tls prefer", []string{"database.tls.mode=prefer"}},
		{"db tls require", []string{"database.tls.mode=require"}},
		{"db tls empty", []string{"database.tls.mode="}},
	}
	for _, tc := range cases {
		sets := append([]string{"-f", vf}, nil...)
		for _, s := range tc.sets {
			sets = append(sets, "--set", s)
		}
		out := renderErrArgs(t, sets...)
		if !strings.Contains(out, "dev.enabled") {
			t.Errorf("%s: must fail mentioning dev.enabled, got: %s", tc.name, out)
		}
		// with dev.enabled the escape hatch works.
		devSets := append(sets, "--set", "dev.enabled=true")
		renderArgs(t, devSets...)
	}
	// hostUsers=false is a hardening, not a bypass — always allowed.
	renderArgs(t, "-f", vf, "--set", "backend.podSecurityContext.hostUsers=false")
	// a benign extraArg passes.
	renderArgs(t, "-f", vf, "--set", "backend.extraArgs[0]=--expiry-interval=45s")
	// verifying sslmodes need no dev gate.
	for _, m := range []string{"verify-ca", "verify-full"} {
		renderArgs(t, "-f", vf, "--set", "database.tls.mode="+m)
	}
}

// TestHardeningDatabaseTLSGate: CHTR-8 chart/backend consistency — the mode
// default is verify-full (always exported as PGSSLMODE), only
// verify-ca/verify-full render without dev.enabled, and a dev-gated
// insecure mode renders the --dev-insecure-db flag the backend needs to
// start.
func TestHardeningDatabaseTLSGate(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	dep := deployment(render(t, "minimal-values.yaml"), "backend")
	env := firstContainerEnv(dep)
	if env["PGSSLMODE"] != "verify-full" {
		t.Errorf("default PGSSLMODE = %q, want verify-full", env["PGSSLMODE"])
	}
	args := strings.Join(firstContainerArgs(dep), "\n")
	if strings.Contains(args, "dev-insecure-db") {
		t.Errorf("default render must not carry --dev-insecure-db\nargs:\n%s", args)
	}
	// dev + insecure mode renders the backend escape hatch.
	docs := renderArgs(t, "-f", vf,
		"--set", "dev.enabled=true", "--set", "database.tls.mode=disable")
	dep = deployment(docs, "backend")
	args = strings.Join(firstContainerArgs(dep), "\n")
	if !strings.Contains(args, "--dev-insecure-db") {
		t.Errorf("dev+insecure mode must render --dev-insecure-db\nargs:\n%s", args)
	}
	env = firstContainerEnv(dep)
	if env["PGSSLMODE"] != "disable" {
		t.Errorf("PGSSLMODE must reflect the dev mode, got %q", env["PGSSLMODE"])
	}
	// dev + a verifying mode does not need the flag.
	docs = renderArgs(t, "-f", vf,
		"--set", "dev.enabled=true", "--set", "database.tls.mode=verify-ca")
	if strings.Contains(strings.Join(firstContainerArgs(deployment(docs, "backend")), "\n"), "dev-insecure-db") {
		t.Error("dev.enabled with a verifying mode must not render --dev-insecure-db")
	}
}

// ---------------------------------------------------------------------------
// CHTR-7 — the deny-all DB peer placeholder must fail the render.
// ---------------------------------------------------------------------------

// TestHardeningStockDefaultsFailOnDBPlaceholder: the stock values.yaml can no
// longer render on its own — database.allowedPeers is effectively required
// when networkPolicy.enabled (the CHTR-7 fix: upgrading with the shipped
// deny-all placeholder silently cut the api from its DB).
func TestHardeningStockDefaultsFailOnDBPlaceholder(t *testing.T) {
	out := renderErrArgs(t)
	if !strings.Contains(out, "database.allowedPeers") || !strings.Contains(out, "deny-all") {
		t.Errorf("bare defaults must fail on the deny-all allowedPeers placeholder, got: %s", out)
	}
}

func TestHardeningDatabasePeersPlaceholder(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	// empty list and the shipped 0.0.0.0/32 placeholder are both denied —
	// the placeholder silently cuts the api from the DB.
	out := renderErrArgs(t, "-f", vf, "--set", "database.allowedPeers=null")
	if !strings.Contains(out, "database.allowedPeers") {
		t.Errorf("empty allowedPeers must fail, got: %s", out)
	}
	out = renderErrArgs(t, "-f", vf,
		"--set", "database.allowedPeers[0].ipBlock.cidr=0.0.0.0/32")
	if !strings.Contains(out, "database.allowedPeers") {
		t.Errorf("deny-all placeholder must fail, got: %s", out)
	}
	// placeholder + a real peer is accepted (the real peer carries it).
	renderArgs(t, "-f", vf,
		"--set", "database.allowedPeers[0].ipBlock.cidr=0.0.0.0/32",
		"--set", "database.allowedPeers[1].ipBlock.cidr=10.20.30.40/32")
	// and the check only applies when networkPolicy is enabled.
	renderArgs(t, "-f", vf,
		"--set", "networkPolicy.enabled=false",
		"--set", "database.allowedPeers=null")
}

// ---------------------------------------------------------------------------
// oidc.requiredGroups -> backend --required-groups (login gate).
// ---------------------------------------------------------------------------

func TestHardeningOIDCRequiredGroups(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	// unset by default — no flag rendered.
	dep := deployment(render(t, "minimal-values.yaml"), "backend")
	for _, a := range firstContainerArgs(dep) {
		if strings.Contains(a, "required-groups") {
			t.Errorf("no --required-groups flag should render with empty oidc.requiredGroups, got %q", a)
		}
	}
	// set → single CSV flag.
	docs := renderArgs(t, "-f", vf,
		"--set", "oidc.requiredGroups[0]=tenant-admin",
		"--set", "oidc.requiredGroups[1]=ops")
	args := firstContainerArgs(deployment(docs, "backend"))
	found := false
	for _, a := range args {
		if a == "--required-groups=tenant-admin,ops" {
			found = true
		}
	}
	if !found {
		t.Errorf("backend args missing --required-groups=tenant-admin,ops\nargs:\n%s", strings.Join(args, "\n"))
	}
	// a comma inside a group name would break the CSV — schema rejects it.
	out := renderErrArgs(t, "-f", vf, "--set", `oidc.requiredGroups[0]=a\,b`)
	if !strings.Contains(out, "requiredGroups") && !strings.Contains(out, "pattern") {
		t.Errorf("comma-containing requiredGroups entry must be rejected, got: %s", out)
	}
}
