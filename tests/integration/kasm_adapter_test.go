//go:build integration

// Contract test for the kasm runtime adapter (approach A,
// docs/kasm-images.md): runs an UNMODIFIED kasmweb/* image with the
// TinyCDI adapter injected exactly the way the operator does it in
// buildPod — an init stage copies the adapter scripts into a shared
// volume, then the runtime container mounts it read-only at /opt/tcdi and
// starts /opt/tcdi/entrypoint.sh with the adapter env contract
// (TCDI_SESSION_CMD, neutralized VNC_PW/VNC_VIEW_ONLY_PW, HOME).
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
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	kasmItLabelValue   = "w15"
	kasmMinFreeBytes   = 10 << 30 // pulling the kasmweb image needs ~4.5 GB
	kasmSessionCmd     = "/usr/bin/chromium-orig --start-maximized about:blank"
	kasmRunTimeout     = 180 * time.Second // cold xfce+kasmvnc boot is slower
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

// runKasmContainer starts the kasm runtime container with the adapter
// injected via initContainer semantics: the shared volume is mounted
// read-only at /opt/tcdi and /opt/tcdi/entrypoint.sh is the container
// command (docker --entrypoint = the pod's command override). The pod
// always mounts a home volume at /home/workspace — for the kasm image
// that mount is REQUIRED (the image has no uid-1000-writable
// /home/workspace in its rootfs), so a missing homeVol is emulated with
// a fresh fsGroup-equivalent volume.
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
		}, extraArgs...)...)
}

// kasmInit runs the adapter image as the init stage: it writes the adapter
// scripts into vol mounted at /opt/tcdi and exits — the same initContainer
// semantics the pod gets (run to completion before the desktop starts).
func kasmInit(t *testing.T, adapterVol string) {
	t.Helper()
	dockerOK(t, "run", "--rm",
		"--label", itLabelKey+"="+kasmItLabelValue,
		"--user", "1000:1000",
		"-v", adapterVol+":/opt/tcdi",
		adapterImage)
	out := mustExecListVol(t, adapterVol)
	for _, f := range []string{"entrypoint.sh", "healthcheck.sh", "xstartup.sh"} {
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
		for _, want := range []string{"entrypoint.sh", "healthcheck.sh", "xstartup.sh"} {
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
		assertNoSecret(t, c, password)
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

		// Poll for a Chromium renderer running as uid 1000 inside a nested
		// user namespace with a seccomp filter engaged (both sandbox
		// layers). TCDI_SESSION_CMD must point at the REAL binary — the
		// image's /usr/bin/chromium wrapper hardcodes --no-sandbox.
		initNS := strings.TrimSpace(mustExec(t, c, "readlink /proc/1/ns/user"))
		var found bool
		deadline := time.Now().Add(120 * time.Second)
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
			t.Fatalf("no Chromium renderer with engaged sandbox (nested userns + Seccomp:2, uid 1000) within 120s\nps:\n%s", ps)
		}

		out := mustExec(t, c, `for p in /proc/[0-9]*/cmdline; do tr '\0' '\n' < "$p" 2>/dev/null | grep -qx -- '--no-sandbox' && echo "$p"; done; true`)
		if strings.TrimSpace(out) != "" {
			t.Fatalf("process running with --no-sandbox: %s", out)
		}
		assertNoSecret(t, c, password)
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
		// kasmpasswd for a DIFFERENT password, and a user config that would
		// move the endpoint off 8443/TLS and point auth at the stale file.
		stale := "stale-password-000"
		mustExec(t, c1, "printf '%s\n' '"+stale+"' > /tmp/wp && rm -f /home/workspace/.kasmpasswd && "+
			"{ cat /tmp/wp; cat /tmp/wp; } | kasmvncpasswd -u kasm_user -w /home/workspace/.kasmpasswd >/dev/null && rm -f /tmp/wp")
		mustExec(t, c1, "printf 'network:\n  websocket_port: 9999\n  ssl:\n    require_ssl: false\nserver:\n  advanced:\n    kasm_password_file: /home/workspace/.kasmpasswd\n' > /home/workspace/.vnc/kasmvnc.yaml")

		dockerOK(t, "rm", "-f", c1)

		c2 := runKasmContainer(t, runID, "restart-b", kasmImage, av, secret, home)
		waitHealthyTimeout(t, c2, kasmRunTimeout)

		mustExec(t, c2, "grep -q persist-me /home/workspace/marker.txt")
		out := mustExec(t, c2, "test -e /tmp/scratch.txt && echo present || echo absent")
		if strings.TrimSpace(out) != "absent" {
			t.Fatalf("rootfs scratch survived container recreation")
		}

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
			logs, _ := docker("logs", "--tail", "40", name)
			t.Fatalf("container %s not running (status=%s):\n%s", name, status, logs)
		}
		if health == "healthy" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	logs, _ := docker("logs", "--tail", "40", name)
	t.Fatalf("container %s did not become healthy in %s\nlogs:\n%s", name, timeout, logs)
}
