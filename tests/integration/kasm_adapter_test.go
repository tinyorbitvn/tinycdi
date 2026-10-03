//go:build integration

// Contract test for the kasm runtime adapter (approach A,
// docs/kasm-images.md): runs an UNMODIFIED kasmweb/* image with the
// TinyCDI adapter injected exactly the way the operator does it in
// buildPod — an init stage copies the adapter scripts into a shared
// volume, then the runtime container mounts it read-only at /opt/tcdi and
// starts /opt/tcdi/entrypoint.sh with the adapter env contract
// (TCDI_SESSION_CMD, neutralized VNC_PW/VNC_VIEW_ONLY_PW, HOME), the
// browser-shim/policy subPath mounts, and readOnlyRootFilesystem.
//
// Image-bound suite, gated on TCDI_IT_KASM_IMAGE (a digest-pinned
// kasmweb/* ref, e.g. kasmweb/chromium@sha256:… — unset = skip). The pull
// is ~1–2 GB compressed / ~4.5 GB uncompressed, so the test first checks
// for >= 10 GiB of free disk and removes the pulled image on exit when it
// did the pulling. The adapter image defaults to tcdi/kasm-adapter:it
// (TCDI_IT_BUILD=1 builds it, like the other runtime images).
//
// Containers/volumes are labelled tcdi.it=w15 and prefixed tcdi-it-w15-;
// everything is removed on exit.
//
//	Run:  TCDI_IT_KASM_IMAGE=kasmweb/chromium@sha256:<digest> \
//	        TCDI_IT_BUILD=1 \
//	        go test -tags=integration ./tests/integration \
//	        -run TestKasmAdapterChromium -v
package integration

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	linuxrt "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

const (
	kasmItLabelValue   = "w15"
	kasmMinFreeBytes   = 10 << 30 // pulling the kasmweb image needs ~4.5 GB
	kasmSessionCmd     = "/usr/bin/chromium-orig --start-maximized about:blank"
	kasmRunTimeout     = 240 * time.Second // cold xfce+kasmvnc boot is slower
	defaultAdapterItIm = "tcdi/kasm-adapter:it"
)

var (
	kasmImage    = os.Getenv("TCDI_IT_KASM_IMAGE") // unset => suite skips
	adapterImage = envOr("TCDI_IT_KASM_ADAPTER_IMAGE", defaultAdapterItIm)
)

// requireFreeDisk skips unless the root filesystem (docker's storage home)
// has at least minFree bytes free.
func requireFreeDisk(t *testing.T, minFree uint64) {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		t.Fatalf("statfs /: %v", err)
	}
	free := st.Bavail * uint64(st.Bsize)
	if free < minFree {
		t.Skipf("not enough free disk for the kasm image pull: %.1f GiB free, want >= %d GiB",
			float64(free)/float64(1<<30), minFree>>30)
	}
}

// pullImageOnce pulls ref unless it already exists locally; returns true
// when the test did the pulling (and must remove it on cleanup).
func pullImageOnce(t *testing.T, ref string) bool {
	t.Helper()
	if _, err := docker("image", "inspect", ref); err == nil {
		return false
	}
	requireFreeDisk(t, kasmMinFreeBytes)
	t.Logf("pulling %s (one image at a time; removed on exit)", ref)
	dockerOK(t, "pull", ref)
	return true
}

// prepVolumeWritable initializes a named volume with uid:gid 1000
// ownership — the docker equivalent of the pod's fsGroup=1000 (which is
// what lets the uid-1000 init container / runtime write the mounts).
func prepVolumeWritable(t *testing.T, vol, mountPath, image string) {
	t.Helper()
	dockerOK(t, "run", "--rm",
		"--label", itLabelKey+"="+kasmItLabelValue,
		"--user", "0", "--entrypoint", "/bin/bash",
		"-v", vol+":"+mountPath,
		image, "-c", "chown 1000:1000 "+mountPath)
}

// kasmAdapterFiles extracts the subPath-mounted adapter files
// (browser-shim.sh, chromium-policy.json) from the adapter volume to a
// host dir so they can be bind-mounted over the image paths — the docker
// equivalent of the pod's subPath mounts.
func kasmAdapterFiles(t *testing.T, adapterVol string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"browser-shim.sh", "chromium-policy.json"} {
		out := dockerOK(t, "run", "--rm",
			"--label", itLabelKey+"="+kasmItLabelValue,
			"-v", adapterVol+":/v:ro",
			"--entrypoint", "/bin/bash",
			kasmImage, "-c", "cat /v/"+name)
		if len(out) == 0 {
			t.Fatalf("adapter volume missing %s", name)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(out), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// kasmMountArgs returns the bind mounts emulating the pod's subPath
// mounts: the sandbox-preserving shim over every Chromium-family wrapper
// and the managed policy into each engine's policy dir — the same path
// lists as buildPod (imported so the test and the pod can never drift).
func kasmMountArgs(filesDir string) []string {
	var args []string
	for _, wp := range linuxrt.AdapterBrowserWrappers {
		args = append(args, "-v",
			filepath.Join(filesDir, "browser-shim.sh")+":"+wp+":ro")
	}
	for _, pp := range linuxrt.AdapterPolicyTargets {
		args = append(args, "-v",
			filepath.Join(filesDir, "chromium-policy.json")+":"+pp+":ro")
	}
	return args
}

// runKasmContainer starts the kasm runtime container with the adapter
// injected via initContainer semantics: the shared volume is mounted
// read-only at /opt/tcdi and /opt/tcdi/entrypoint.sh is the container
// command (docker --entrypoint = the pod's command override). The pod
// always mounts a home volume at /home/workspace — for the kasm image
// that mount is REQUIRED (the image has no uid-1000-writable
// /home/workspace in its rootfs), so a missing homeVol is emulated with
// a fresh fsGroup-equivalent volume. The container mirrors the pod's
// readOnlyRootFilesystem with a tmpfs /tmp like the pod's emptyDir.
func runKasmContainer(t *testing.T, runID, name, image, adapterVol, secretDir, homeVol string, extraArgs ...string) string {
	t.Helper()
	if homeVol == "" {
		homeVol = newVolume(t, runID, name+"-home")
		prepVolumeWritable(t, homeVol, "/home/workspace", image)
	}
	return runContainer(t, runID, name, image, secretDir, homeVol,
		append([]string{
			"--user", "1000:1000",
			"--entrypoint", "/opt/tcdi/entrypoint.sh",
			"-v", adapterVol + ":/opt/tcdi:ro",
			// readOnlyRootFilesystem: true (KASM-6/7) — everything the
			// session writes lands on a mount, like in the pod.
			"--read-only",
			"--tmpfs", "/tmp:rw,exec,mode=1777",
			// The pod's exec readinessProbe runs the adapter healthcheck —
			// override whatever HEALTHCHECK the kasmweb image bakes in.
			"--health-cmd", "/opt/tcdi/healthcheck.sh",
			"--health-interval", "5s",
			"--health-timeout", "5s",
			"--health-retries", "12",
			"--health-start-period", "5s",
			"-e", "HOME=/home/workspace",
			"-e", "TCDI_SESSION_CMD=" + kasmSessionCmd,
			"-e", "VNC_PW=",
			"-e", "VNC_VIEW_ONLY_PW=",
		}, append(kasmMountArgs(kasmAdapterFiles(t, adapterVol)), extraArgs...)...)...)
}

// kasmInit runs the adapter image as the init stage: it writes the adapter
// scripts into vol mounted at /opt/tcdi and exits — the same initContainer
// semantics the pod gets (run to completion before the desktop starts).
func kasmInit(t *testing.T, adapterVol string) {
	t.Helper()
	dockerOK(t, "run", "--rm",
		"--label", itLabelKey+"="+kasmItLabelValue,
		"--user", "1000:1000",
		"--read-only", // the pod's initContainer runs with a ro rootfs too
		"-v", adapterVol+":/opt/tcdi",
		adapterImage)
	out := mustExecListVol(t, adapterVol)
	for _, f := range []string{"entrypoint.sh", "healthcheck.sh", "xstartup.sh",
		"browser-shim.sh", "chromium-policy.json"} {
		if !strings.Contains(out, f) {
			t.Fatalf("adapter init did not install %s into the shared volume; contents:\n%s", f, out)
		}
	}
}

// mustExecListVol lists the volume root via a throwaway container on the
// (already-pulled) kasm image — avoids another image pull.
func mustExecListVol(t *testing.T, vol string) string {
	t.Helper()
	out := dockerOK(t, "run", "--rm",
		"--label", itLabelKey+"="+kasmItLabelValue,
		"-v", vol+":/v:ro",
		"--entrypoint", "/bin/bash",
		kasmImage, "-c", "ls -la /v")
	return out
}

// sessionEnv returns the DISPLAY/DBUS exports a docker exec needs to act
// as a session user (launchers resolve the desktop via these).
func sessionEnv() string {
	return "export DISPLAY=:1; " +
		"export DBUS_SESSION_BUS_ADDRESS=$(tr '\\0' '\\n' < /proc/1/environ 2>/dev/null | " +
		"sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p'); " +
		"[ -n \"$DBUS_SESSION_BUS_ADDRESS\" ] || " +
		"export DBUS_SESSION_BUS_ADDRESS=$(for p in $(pgrep -f 'xfwm4|chromium|Xvnc' | head -5); do " +
		"v=$(tr '\\0' '\\n' < /proc/$p/environ 2>/dev/null | sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p'); " +
		"[ -n \"$v\" ] && { echo \"$v\"; break; }; done); "
}

// assertSandboxedRenderers fails unless EVERY chromium-family renderer is
// inside a nested user namespace AND carries more seccomp filters than
// pid 1 (the container's RuntimeDefault baseline = 1; a sandboxed
// renderer stacks the renderer filter on top). A --no-sandbox renderer
// fails both checks.
func assertSandboxedRenderers(t *testing.T, c string) {
	t.Helper()
	initNS := strings.TrimSpace(mustExec(t, c, "readlink /proc/1/ns/user"))
	initFilters, err := strconv.Atoi(strings.TrimSpace(mustExec(t, c,
		"awk '/^Seccomp_filters:/{print $2}' /proc/1/status")))
	if err != nil {
		t.Fatalf("pid 1 Seccomp_filters unreadable: %v", err)
	}
	// Renderers churn: a pid pgrep found may exit before its status is
	// read. Pids that vanish are skipped; a LIVE renderer that fails a
	// check is a hard failure.
	checked := 0
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := mustExec(t, c, "pgrep -f 'chromium.*--type=renderer|chrome.*--type=renderer' || true")
		for _, pid := range strings.Fields(out) {
			status := mustExec(t, c, "cat /proc/"+pid+"/status 2>/dev/null || true")
			if status == "" {
				continue // renderer exited between pgrep and read
			}
			if !strings.Contains(status, "Seccomp:\t2") && !strings.Contains(status, "Seccomp: 2") {
				t.Fatalf("renderer pid %s is not under seccomp filter mode:\n%s", pid, status)
			}
			f, err := strconv.Atoi(strings.TrimSpace(mustExec(t, c,
				"awk '/^Seccomp_filters:/{print $2}' /proc/"+pid+"/status 2>/dev/null || echo 0")))
			if err != nil {
				continue // died
			}
			if f <= initFilters {
				t.Fatalf("renderer pid %s stacks no filter on top of the container baseline (%d vs pid1's %d)", pid, f, initFilters)
			}
			ns := strings.TrimSpace(mustExec(t, c, "readlink /proc/"+pid+"/ns/user 2>/dev/null || true"))
			if ns == "" {
				continue // died
			}
			if ns == initNS {
				t.Fatalf("renderer pid %s runs in the init user namespace (no sandbox)", pid)
			}
			checked++
		}
		if checked > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if checked == 0 {
		t.Fatal("no live renderer processes found")
	}
}

// assertNoSandboxFlags fails if ANY process carries a sandbox-killing
// flag. Chromium rewrites its process title to a single space-joined
// cmdline, so flags are matched on whitespace AND NUL boundaries.
func assertNoSandboxFlags(t *testing.T, c string) {
	t.Helper()
	// NB: --no-zygote-sandbox and --service-sandbox-type=none are omitted
	// — Chromium adds them internally (bootstrap zygote / network
	// utility) in a healthy sandboxed configuration.
	out := mustExec(t, c, `for p in /proc/[0-9]*/cmdline; do
  tr '\0' ' ' < "$p" 2>/dev/null | grep -Eq '(^| )--(no-sandbox|disable-setuid-sandbox|disable-namespace-sandbox|disable-seccomp-filter-sandbox|disable-gpu-sandbox|single-process|in-process-gpu)( |$)' && echo "$p"
done; true`)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("process(es) running with sandbox-killing flags: %s", out)
	}
}

// waitSandboxedRenderer polls until at least one sandboxed renderer
// exists, then asserts ALL of them (plus the flag sweep).
func waitSandboxedRenderer(t *testing.T, c string, timeout time.Duration) {
	t.Helper()
	initNS := strings.TrimSpace(mustExec(t, c, "readlink /proc/1/ns/user"))
	var found bool
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && !found {
		out, err := execIn(t, c, "pgrep -f 'chromium.*--type=renderer' || true")
		if err != nil {
			t.Fatalf("renderer pgrep: %v", err)
		}
		for _, pid := range strings.Fields(out) {
			status := mustExec(t, c, "cat /proc/"+pid+"/status 2>/dev/null || true")
			if !strings.Contains(status, "Seccomp:\t2") &&
				!strings.Contains(status, "Seccomp: 2") {
				continue
			}
			if !strings.Contains(status, "Uid:\t1000\t") &&
				!strings.Contains(status, "Uid: 1000 ") {
				continue
			}
			ns := strings.TrimSpace(mustExec(t, c, "readlink /proc/"+pid+"/ns/user 2>/dev/null || true"))
			if ns != "" && ns != initNS {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(3 * time.Second)
		}
	}
	if !found {
		ps, _ := execIn(t, c, "ps aux | head -40")
		t.Fatalf("no Chromium renderer with engaged sandbox (nested userns + Seccomp:2, uid 1000) within %s\nps:\n%s", timeout, ps)
	}
	assertSandboxedRenderers(t, c)
	assertNoSandboxFlags(t, c)
}

func TestKasmAdapterChromium(t *testing.T) {
	if kasmImage == "" {
		t.Skip("TCDI_IT_KASM_IMAGE unset — set a digest-pinned kasmweb/* ref to run the adapter contract")
	}
	if _, err := os.Stat(seccompFile); err != nil {
		t.Fatalf("runtime seccomp profile missing: %s", seccompFile)
	}
	requireImage(t, adapterImage, "build/kasm-adapter")
	pulled := pullImageOnce(t, kasmImage)

	runBytes := make([]byte, 4)
	if _, err := rand.Read(runBytes); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("%x", runBytes)

	t.Cleanup(func() {
		out, _ := docker("ps", "-aq", "--filter", "label=tcdi.it.run="+runID)
		for _, c := range strings.Fields(out) {
			docker("rm", "-f", c) //nolint:errcheck
		}
		vols, _ := docker("volume", "ls", "-q", "--filter", "label=tcdi.it.run="+runID)
		for _, v := range strings.Fields(vols) {
			docker("volume", "rm", "-f", v) //nolint:errcheck
		}
		if pulled {
			// The kasm image is pulled for this test only — leave no
			// multi-GB image behind on the runner.
			docker("rmi", "-f", kasmImage) //nolint:errcheck
		}
	})

	// The adapter volume: created once per subtest, populated by the init
	// stage, mounted read-only into the runtime container.
	adapterVol := func() string {
		v := newVolume(t, runID, "adapter")
		prepVolumeWritable(t, v, "/opt/tcdi", kasmImage)
		kasmInit(t, v)
		return v
	}

	t.Run("InitInstallsAdapterScripts", func(t *testing.T) {
		v := adapterVol()
		out := mustExecListVol(t, v)
		for _, want := range []string{"entrypoint.sh", "healthcheck.sh", "xstartup.sh",
			"browser-shim.sh", "chromium-policy.json"} {
			if !strings.Contains(out, want) {
				t.Fatalf("adapter volume missing %s:\n%s", want, out)
			}
		}
		// Scripts must be executable (0755) for the exec probe and command.
		if !strings.Contains(out, "-rwxr-xr-x") {
			t.Fatalf("adapter scripts not executable:\n%s", out)
		}
	})

	t.Run("ReadinessEndpointAndAuth", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "desktop", kasmImage, adapterVol(), secret, "")
		waitHealthyTimeout(t, c, kasmRunTimeout)

		code, err := httpsGet(t, c, "", "")
		if err != nil {
			t.Fatalf("endpoint did not answer over TLS: %v", err)
		}
		if code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("anonymous request: got %d, want 401/403", code)
		}
		code, err = httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("auth with mounted secret: code=%d err=%v, want 200", code, err)
		}
		code, err = httpsGet(t, c, "kasm_user", "definitely-wrong")
		if err == nil && code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("wrong password accepted: %d", code)
		}

		// Non-root runtime + bounded shm.
		if out := strings.TrimSpace(mustExec(t, c, "id -u")); out != "1000" {
			t.Fatalf("runtime uid = %s, want 1000", out)
		}
		shm := dockerOK(t, "inspect", "-f", "{{.HostConfig.ShmSize}}", c)
		if strings.TrimSpace(shm) != "268435456" {
			t.Fatalf("ShmSize = %s, want 268435456 (256m bounded mount)", shm)
		}
		// readOnlyRootFilesystem (KASM-6/7): the rootfs rejects writes.
		if ro := strings.TrimSpace(dockerOK(t, "inspect", "-f", "{{.HostConfig.ReadonlyRootfs}}", c)); ro != "true" {
			t.Fatalf("ReadonlyRootfs = %s, want true", ro)
		}
		if out := strings.TrimSpace(mustExec(t, c,
			"touch /etc/x 2>/dev/null && echo wrote || echo ro")); out != "ro" {
			t.Fatalf("rootfs writable: %s", out)
		}
		// The writable-inside-webroot path (KASM-6): Downloads is a symlink
		// into the rootfs home — under a ro rootfs nothing can plant a
		// symlink there, so /Downloads/<link>/… traversal is dead.
		if out := strings.TrimSpace(mustExec(t, c,
			"ln -s / /home/kasm-user/Downloads/pwn 2>/dev/null && echo planted || echo ro")); out != "ro" {
			t.Fatalf("web-root-adjacent dir writable: %s", out)
		}
		// KASM-9: KasmVNC 1.4.x cannot disable the UDP transport (it binds
		// :8443/udp like the native images — the TCP-only NetworkPolicy is
		// the control). What the contract can assert: no OTHER UDP socket
		// may be wildcard-bound (client sockets on a specific addr are
		// normal transient noise).
		if out := strings.TrimSpace(mustExec(t, c,
			`awk 'NR>1 {split($2,a,":"); if (a[1] ~ /^0+$/ && a[2] != "20FB") print FILENAME": "$2}' /proc/net/udp /proc/net/udp6 2>/dev/null`)); out != "" {
			t.Fatalf("unexpected wildcard-bound UDP listeners besides Xvnc :8443: %s", out)
		}
		// Owner-gated management routes stay closed for the write-only user.
		if out := strings.TrimSpace(mustExec(t, c,
			"curl -sk -o /dev/null -w '%{http_code}' -u kasm_user:$(cat /run/secrets/tcdi/password) https://127.0.0.1:8443/api/get_users || true")); out != "401" {
			t.Fatalf("/api/get_users with valid creds = %s, want 401 (write-only user)", out)
		}
		// Adapter mounts are read-only inside the runtime container.
		if out := strings.TrimSpace(mustExec(t, c,
			"touch /opt/tcdi/x 2>/dev/null && echo wrote || echo ro")); out != "ro" {
			t.Fatalf("/opt/tcdi is writable in the runtime container: %s", out)
		}
		// The shim mounts are read-only too (KASM-1): the session user
		// cannot remove or rewrite them (CAP_SYS_ADMIN is dropped, and the
		// mounts are ro either way).
		if out := strings.TrimSpace(mustExec(t, c,
			"rm -f /usr/bin/chromium 2>/dev/null; test -x /usr/bin/chromium && echo present || echo gone")); out != "present" {
			t.Fatalf("wrapper shim removable/replaced: %s", out)
		}
		if out := strings.TrimSpace(mustExec(t, c,
			"umount /usr/bin/chromium 2>/dev/null && echo unmounted || echo held")); out != "held" {
			t.Fatalf("shim mount unmountable by session user: %s", out)
		}
		// Managed policy landed (KASM-8).
		mustExec(t, c, "grep -q CommandLineFlagSecurityWarningsEnabled /etc/chromium/policies/managed/zz-tcdi.json")
		assertNoSecret(t, c, password)
	})

	t.Run("ReadinessProbeIsNotAnAuthFailure", func(t *testing.T) {
		// Same contract as the runtime images: the exec readiness probe must
		// not register as an authentication failure, or KasmVNC's
		// brute-force protection blacklists loopback after a few cycles.
		secret, _ := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "probe", kasmImage, adapterVol(), secret, "")
		waitHealthyTimeout(t, c, kasmRunTimeout)
		for i := 0; i < 20; i++ {
			if _, err := execIn(t, c, "/opt/tcdi/healthcheck.sh"); err != nil {
				t.Fatalf("probe cycle %d not ready: %v", i+1, err)
			}
		}
		log := mustExec(t, c, "cat /home/workspace/.vnc/*.log")
		for _, bad := range []string{"blacklisted", "Authentication attempt failed"} {
			if strings.Contains(log, bad) {
				t.Fatalf("20 probe cycles (plus the HEALTHCHECK) left %q in the KasmVNC log:\n%s", bad, log)
			}
		}
		code := strings.TrimSpace(mustExec(t, c,
			`curl -sk -o /dev/null -w '%{http_code}' -u "kasm_user:$(cat /run/secrets/tcdi/password)" https://127.0.0.1:8443/`))
		if code != "200" {
			t.Fatalf("loopback after probing: got %s, want 200", code)
		}
	})

	t.Run("DisplayDeathNotReady", func(t *testing.T) {
		secret, _ := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "death", kasmImage, adapterVol(), secret, "")
		waitHealthyTimeout(t, c, kasmRunTimeout)

		mustExec(t, c, "pkill -x Xvnc")
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			status, health := containerState(t, c)
			if status != "running" || health == "unhealthy" {
				return
			}
			time.Sleep(1 * time.Second)
		}
		status, health := containerState(t, c)
		t.Fatalf("display killed but container still ready: status=%s health=%s", status, health)
	})

	t.Run("BrowserSandbox", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "browser", kasmImage, adapterVol(), secret, "")
		waitHealthyTimeout(t, c, kasmRunTimeout)

		code, err := httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("endpoint auth: code=%d err=%v, want 200", code, err)
		}

		// Every renderer must be inside the nested-userns + stacked
		// seccomp sandbox, and no process may carry a sandbox-killing
		// flag. TCDI_SESSION_CMD points at the REAL binary — the image's
		// /usr/bin/chromium wrapper hardcodes --no-sandbox.
		waitSandboxedRenderer(t, c, 180*time.Second)
		assertNoSecret(t, c, password)
	})

	// KASM-1: every way the session can (re)launch the browser must keep
	// the sandbox. The wrapper shim covers the binary paths; launches
	// with a FRESH user-data-dir force a brand-new browser process tree
	// (no single-instance handoff), so a regression would really produce
	// unsandboxed renderers.
	t.Run("RelaunchPathsStaySandboxed", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "relaunch", kasmImage, adapterVol(), secret, "")
		waitHealthyTimeout(t, c, kasmRunTimeout)
		waitSandboxedRenderer(t, c, 180*time.Second)

		launch := func(desc, cmd string) {
			t.Helper()
			// (setsid … &) so the launcher survives the exec's exit.
			mustExec(t, c, sessionEnv()+"( setsid "+cmd+" </dev/null >/tmp/launch.out 2>&1 & ); sleep 1")
			t.Logf("launched via %s", desc)
		}

		// Binary + alternatives paths, each a fresh profile dir so a new
		// browser process actually spawns through the shim.
		launch("/usr/bin/chromium", "/usr/bin/chromium --user-data-dir=/tmp/rl1 --no-first-run about:blank")
		launch("x-www-browser", "x-www-browser --user-data-dir=/tmp/rl2 --no-first-run about:blank")
		launch("sensible-browser", "sensible-browser --user-data-dir=/tmp/rl3 --no-first-run about:blank")
		// Even a user explicitly passing --no-sandbox gets it stripped.
		launch("explicit --no-sandbox", "/usr/bin/chromium --no-sandbox --user-data-dir=/tmp/rl4 --no-first-run about:blank")
		// Desktop-file paths (best-effort — needs a session bus).
		launch("gtk-launch chromium.desktop", "gtk-launch chromium.desktop https://example.com || true")
		launch("gio launch ~/Desktop/chromium.desktop", "gio launch /home/workspace/Desktop/chromium.desktop || true")

		// Prove each binary-path launch actually spawned a NEW browser
		// (its profile dir appears in a live process' cmdline) — then that
		// every renderer is sandboxed and no sandbox-killing flag ran.
		for _, dir := range []string{"/tmp/rl1", "/tmp/rl2", "/tmp/rl3", "/tmp/rl4"} {
			deadline := time.Now().Add(60 * time.Second)
			spawned := false
			for time.Now().Before(deadline) && !spawned {
				out := mustExec(t, c, "pgrep -f 'chromiu[m].*"+dir+"' || true")
				spawned = strings.TrimSpace(out) != ""
				if !spawned {
					time.Sleep(2 * time.Second)
				}
			}
			if !spawned {
				logs, _ := execIn(t, c, "cat /tmp/launch.out; ps aux | head -30")
				t.Fatalf("no browser spawned with %s (launch path broken, not just unsandboxed):\n%s", dir, logs)
			}
		}
		assertSandboxedRenderers(t, c)
		assertNoSandboxFlags(t, c)
		assertNoSecret(t, c, password)
	})

	// KASM-1 lifecycle: when the session payload exits the display must
	// die with it — no lingered desktop to relaunch through.
	t.Run("SessionEndsWithPayload", func(t *testing.T) {
		secret, _ := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "session-end", kasmImage, adapterVol(), secret, "")
		waitHealthyTimeout(t, c, kasmRunTimeout)
		waitSandboxedRenderer(t, c, 180*time.Second)

		// User closes the browser. ([m] keeps pkill from matching this
		// exec's own sh -c cmdline.)
		mustExec(t, c, "pkill -f '/usr/lib/chromium/chromiu[m]' || true")
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			status, _ := containerState(t, c)
			if status != "running" {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("container still running after payload exit — the desktop session must die with the browser\nlogs:\n%s", dockerCombined("logs", "--tail", "30", c))
	})

	t.Run("RestartHomeAndStaleCredential", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		home := newVolume(t, runID, "home")
		prepVolumeWritable(t, home, "/home/workspace", kasmImage)
		av := adapterVol()

		c1 := runKasmContainer(t, runID, "restart-a", kasmImage, av, secret, home)
		waitHealthyTimeout(t, c1, kasmRunTimeout)

		mustExec(t, c1, "echo persist-me > /home/workspace/marker.txt")
		mustExec(t, c1, "echo scratch > /tmp/scratch.txt")

		// Plant stale artifacts on the persistent home: a valid-format
		// kasmpasswd for a DIFFERENT password, a user config that would
		// move the endpoint off 8443/TLS, and a symlinked kasmvnc.yaml
		// (KASM-10 — must never be followed).
		stale := "stale-password-000"
		mustExec(t, c1, "printf '%s\n' '"+stale+"' > /tmp/wp && rm -f /home/workspace/.kasmpasswd && "+
			"{ cat /tmp/wp; cat /tmp/wp; } | kasmvncpasswd -u kasm_user -w /home/workspace/.kasmpasswd >/dev/null && rm -f /tmp/wp")
		mustExec(t, c1, "rm -rf /home/workspace/.vnc && ln -s /dev/null /home/workspace/.vnc")

		dockerOK(t, "rm", "-f", c1)

		c2 := runKasmContainer(t, runID, "restart-b", kasmImage, av, secret, home)
		waitHealthyTimeout(t, c2, kasmRunTimeout)

		mustExec(t, c2, "grep -q persist-me /home/workspace/marker.txt")
		out := mustExec(t, c2, "test -e /tmp/scratch.txt && echo present || echo absent")
		if strings.TrimSpace(out) != "absent" {
			t.Fatalf("rootfs scratch survived container recreation")
		}
		// The planted ~/.vnc symlink must have been replaced by a real dir.
		out = mustExec(t, c2, "test -L /home/workspace/.vnc && echo symlink || echo dir")
		if strings.TrimSpace(out) != "dir" {
			t.Fatalf("planted ~/.vnc symlink survived boot: %s", out)
		}
		mustExec(t, c2, "grep -q 'websocket_port: 8443' /home/workspace/.vnc/kasmvnc.yaml")

		code, err := httpsGet(t, c2, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("mounted secret rejected after restart: code=%d err=%v", code, err)
		}
		code, err = httpsGet(t, c2, "kasm_user", stale)
		if err == nil && code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("stale in-home credential accepted: code=%d", code)
		}
		assertNoSecret(t, c2, password)
	})
}

// waitHealthyTimeout is waitHealthy with a caller-chosen deadline — the
// kasm desktop boots slower than the tcdi images.
func waitHealthyTimeout(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, health := containerState(t, name)
		if status != "running" && status != "created" {
			t.Fatalf("container %s not running (status=%s):\n%s", name, status, deadContainerDiag(t, name))
		}
		if health == "healthy" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("container %s did not become healthy in %s\n%s", name, timeout, deadContainerDiag(t, name))
}
