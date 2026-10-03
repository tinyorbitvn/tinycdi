//go:build integration

// Contract test for the Linux runtime images (design §7).
//
// The images are a base (tcdi/linux-base: KasmVNC, X, entrypoint,
// hardening) and two profiles built FROM it (tcdi/linux-desktop: XFCE4;
// tcdi/browser: openbox kiosk + browsers). The contract tests below run
// against the desktop profile and the browser profile; the base image
// itself is checked for the same endpoint contract in BaseImage. The
// desktop experience has its own file (linux_desktop_test.go).
//
// Covers: HTTPS streaming on container port 8443, healthcheck that reports
// ready only while the X display AND the KasmVNC endpoint answer, mounted
// Secret credentials (never env/logs), persistent /home/workspace vs
// ephemeral rootfs, stale in-home credential/config files never overriding
// the mounted Secret, non-root uid, bounded /dev/shm, and the browser
// profile launching Chromium with its sandbox engaged (no --no-sandbox;
// local Docker uses the allowlist seccomp
// profile in tests/integration/testdata/seccomp-runtime.json in place of
// the node Localhost seccomp+AppArmor pair).
//
// Drives Docker through the CLI (no SDK). Containers/volumes are labelled
// tcdi.it=w1t2 and prefixed tcdi-it-w1t2-; everything is removed on exit.
//
// Run:  go test -tags=integration ./tests/integration -run TestLinuxRuntimeReadinessAndHome -v
// Requires the images to be built first (see docs/images.md),
// or set TCDI_IT_BUILD=1 to let the test build missing images.

package integration

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	itLabelKey    = "tcdi.it"
	itLabelValue  = "w1t2"
	namePrefix    = "tcdi-it-w1t2-"
	containerPort = "8443"
	runTimeout    = 120 * time.Second
)

var (
	repoRoot    = mustRepoRoot()
	seccompFile = filepath.Join(repoRoot, "tests", "integration", "testdata", "seccomp-runtime.json")

	baseImage    = envOr("TCDI_IT_BASE_IMAGE", "tcdi/linux-base:it")
	desktopImage = envOr("TCDI_IT_DESKTOP_IMAGE", "tcdi/linux-desktop:it")
	browserImage = envOr("TCDI_IT_BROWSER_IMAGE", "tcdi/browser:it")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustRepoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		panic(err)
	}
	return root
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}

// dockerCombined runs docker with stdout and stderr merged into one
// buffer — for diagnostics only. `docker logs` prints the container's
// stderr on the CLI's stderr, so docker() alone would silently drop the
// entrypoint's failure message.
func dockerCombined(args ...string) string {
	cmd := exec.Command("docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(&out, "(docker %s failed: %v)", strings.Join(args, " "), err)
	}
	return out.String()
}

func dockerOK(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(args...)
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// requireImage fails (the red state) until the runtime images exist locally.
// Set TCDI_IT_BUILD=1 to build missing images from build/<name> (repo-root
// context) instead. The profile images (build/linux-desktop, build/browser)
// are built FROM the local base image, so asking for one also ensures the
// base.
func requireImage(t *testing.T, image, imageDir string) {
	t.Helper()
	var buildArgs []string
	if imageDir != "build/linux-base" && (imageDir == "build/linux-desktop" || imageDir == "build/browser") {
		requireImage(t, baseImage, "build/linux-base")
		buildArgs = []string{"--build-arg", "BASE_IMAGE=" + baseImage}
	}
	if _, err := docker("image", "inspect", image); err == nil {
		return
	}
	if os.Getenv("TCDI_IT_BUILD") != "1" {
		t.Fatalf("image %s not built; run: docker build -f %s/Dockerfile -t %s . (or set TCDI_IT_BUILD=1)",
			image, imageDir, image)
	}
	t.Logf("building missing image %s from %s/Dockerfile", image, imageDir)
	args := append([]string{"build", "-f", filepath.Join(repoRoot, imageDir, "Dockerfile"), "-t", image}, buildArgs...)
	dockerOK(t, append(args, repoRoot)...)
}

// selfSignedSecret builds a secret dir holding password/username/tls.crt/
// tls.key - the same file layout the pod Secret volume presents.
func selfSignedSecret(t *testing.T) (dir, password string) {
	t.Helper()
	dir = t.TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tcdi-it"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	password = base64.RawURLEncoding.EncodeToString(raw)
	writeFile(t, filepath.Join(dir, "password"), []byte(password+"\n"))
	writeFile(t, filepath.Join(dir, "username"), []byte("kasm_user\n"))
	// A pod Secret volume is root-owned 0755/0644 (defaultMode): the
	// runtime uid can traverse and read it no matter which uid launched
	// the container. Mirror that — the test user's uid differs across
	// environments (uid 1000 workstations vs the uid-1001 CI runner) and
	// the host defaults (0700 dir, 0600 files) would make the mount
	// unreadable inside the uid-1000 container.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tls.crt", "tls.key", "password", "username"} {
		if err := os.Chmod(filepath.Join(dir, f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, password
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// probeSecretDir builds a minimal Secret dir (username + password only —
// the probe reads nothing else) whose files carry the given line ending,
// with pod-Secret-volume permissions. A probe run can then be pointed at
// it via the healthcheck's TCDI_SECRET_DIR override.
func probeSecretDir(t *testing.T, user, password, ending string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "password"), []byte(password+ending))
	writeFile(t, filepath.Join(dir, "username"), []byte(user+ending))
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"password", "username"} {
		if err := os.Chmod(filepath.Join(dir, f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// emptySecretDir builds a Secret dir whose password file exists but is
// empty — the probe must fail closed on it.
func emptySecretDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "password"), nil)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "password"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// containerRunArgs records each container's docker run argv so a dead
// container's diagnostics can show exactly how it was launched.
var containerRunArgs = map[string]string{}

// deadContainerDiag dumps everything useful about a container that is not
// running: the docker run argv it was launched with, its State object
// (exit code, OOM, runtime error), and the tail of its logs with both
// streams merged (see dockerCombined).
func deadContainerDiag(t *testing.T, name string) string {
	t.Helper()
	var b strings.Builder
	if args, ok := containerRunArgs[name]; ok {
		fmt.Fprintf(&b, "docker %s\n", args)
	}
	fmt.Fprintf(&b, "state: %s\n",
		strings.TrimSpace(dockerCombined("inspect", "-f", "{{json .State}}", name)))
	fmt.Fprintf(&b, "logs (stdout+stderr):\n%s", dockerCombined("logs", "--tail", "80", name))
	return b.String()
}

// runContainer starts a hardened runtime container:
// non-root image user, all caps dropped, no-new-privileges, the allowlist
// seccomp profile (local stand-in for the node Localhost pair), bounded /dev/shm,
// credentials as a read-only file mount, /run/tcdi as ephemeral tmpfs.
func runContainer(t *testing.T, runID, name, image, secretDir, homeVol string, extraArgs ...string) string {
	t.Helper()
	full := namePrefix + runID + "-" + name
	args := []string{
		"run", "-d", "--name", full,
		"--label", itLabelKey + "=" + itLabelValue,
		"--label", "tcdi.it.run=" + runID,
		"-p", "127.0.0.1:0:" + containerPort,
		"-v", secretDir + ":/run/secrets/tcdi:ro",
		"--tmpfs", "/run/tcdi:rw,exec,uid=1000,gid=1000,mode=700",
		"--shm-size", "256m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--security-opt", "seccomp=" + seccompFile,
	}
	if homeVol != "" {
		args = append(args, "-v", homeVol+":/home/workspace")
	}
	args = append(args, extraArgs...)
	args = append(args, image)
	containerRunArgs[full] = strings.Join(args, " ")
	dockerOK(t, args...)
	t.Cleanup(func() {
		docker("rm", "-f", full) //nolint:errcheck
	})
	return full
}

func newVolume(t *testing.T, runID, name string) string {
	t.Helper()
	vol := namePrefix + runID + "-" + name
	dockerOK(t, "volume", "create",
		"--label", itLabelKey+"="+itLabelValue,
		"--label", "tcdi.it.run="+runID, vol)
	t.Cleanup(func() {
		docker("volume", "rm", "-f", vol) //nolint:errcheck
	})
	return vol
}

func containerState(t *testing.T, name string) (status, health string) {
	t.Helper()
	out := dockerOK(t, "inspect", "-f",
		"{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", name)
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) == 0 {
		return "", ""
	}
	status = parts[0]
	if len(parts) > 1 {
		health = parts[1]
	}
	return status, health
}

func hostPort(t *testing.T, name string) string {
	t.Helper()
	out := dockerOK(t, "inspect", "-f",
		fmt.Sprintf("{{(index (index .NetworkSettings.Ports \"%s/tcp\") 0).HostPort}}", containerPort),
		name)
	return strings.TrimSpace(out)
}

func waitHealthy(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(runTimeout)
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
	t.Fatalf("container %s did not become healthy in %s\n%s", name, runTimeout, deadContainerDiag(t, name))
}

func httpsGet(t *testing.T, name, user, password string) (int, error) {
	t.Helper()
	port := hostPort(t, name)
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	cl := &http.Client{Transport: tr, Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET", "https://127.0.0.1:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func execIn(t *testing.T, name string, shell string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", "exec", name, "sh", "-c", shell)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}

func mustExec(t *testing.T, name, shell string) string {
	t.Helper()
	out, err := execIn(t, name, shell)
	if err != nil {
		t.Fatalf("exec %q in %s: %v", shell, name, err)
	}
	return out
}

// assertNoSecret scans container logs and inspect env for the password.
func assertNoSecret(t *testing.T, name, password string) {
	t.Helper()
	logs := dockerCombined("logs", name)
	if strings.Contains(logs, password) {
		t.Fatalf("password leaked into container logs of %s", name)
	}
	env := dockerOK(t, "inspect", "-f", "{{json .Config.Env}}", name)
	if strings.Contains(env, password) {
		t.Fatalf("password leaked into container env of %s", name)
	}
	if strings.Contains(env, "PASSWORD") || strings.Contains(env, "passwd") {
		t.Fatalf("credential-looking env var present on %s: %s", name, env)
	}
}

func TestLinuxRuntimeReadinessAndHome(t *testing.T) {
	if _, err := os.Stat(seccompFile); err != nil {
		t.Fatalf("runtime seccomp profile missing: %s", seccompFile)
	}
	requireImage(t, baseImage, "build/linux-base")
	requireImage(t, desktopImage, "build/linux-desktop")
	requireImage(t, browserImage, "build/browser")

	runBytes := make([]byte, 4)
	if _, err := rand.Read(runBytes); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("%x", runBytes)

	// Belt-and-suspenders cleanup for anything this run leaked.
	t.Cleanup(func() {
		out, _ := docker("ps", "-aq", "--filter", "label=tcdi.it.run="+runID)
		for _, c := range strings.Fields(out) {
			docker("rm", "-f", c) //nolint:errcheck
		}
		vols, _ := docker("volume", "ls", "-q", "--filter", "label=tcdi.it.run="+runID)
		for _, v := range strings.Fields(vols) {
			docker("volume", "rm", "-f", v) //nolint:errcheck
		}
	})

	t.Run("ReadinessEndpointAndAuth", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		c := runContainer(t, runID, "desktop", desktopImage, secret, "")
		waitHealthy(t, c)

		// Endpoint answers over TLS; anonymous gets an auth challenge.
		code, err := httpsGet(t, c, "", "")
		if err != nil {
			t.Fatalf("endpoint did not answer over TLS: %v", err)
		}
		if code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("anonymous request: got %d, want 401/403", code)
		}
		// Mounted-Secret credential authenticates; a wrong password does not.
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
		assertNoSecret(t, c, password)
	})

	t.Run("BaseImage", func(t *testing.T) {
		// The base alone satisfies the endpoint contract (a profile only
		// replaces the session): healthy, authenticated, non-root, no
		// window manager and no browser installed in it.
		secret, password := selfSignedSecret(t)
		c := runContainer(t, runID, "base", baseImage, secret, "")
		waitHealthy(t, c)
		code, err := httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("base image endpoint auth: code=%d err=%v, want 200", code, err)
		}
		if out := strings.TrimSpace(mustExec(t, c, "id -u")); out != "1000" {
			t.Fatalf("base runtime uid = %s, want 1000", out)
		}
		out := mustExec(t, c, "for b in openbox xterm xfce4-session chromium firefox-esr; do command -v $b; done; true")
		if strings.TrimSpace(out) != "" {
			t.Fatalf("base image ships profile software: %q", out)
		}
		assertNoSecret(t, c, password)
	})

	t.Run("FontsCoverVietnameseAndCJK", func(t *testing.T) {
		// Fonts live in linux-base, so both profiles inherit them. A page
		// with Vietnamese or CJK text must not render tofu: fontconfig
		// (the fallback mechanism Firefox and Chromium use per glyph) must
		// resolve a font that actually carries the codepoints for each
		// generic family. fc-match picks the font a request resolves to;
		// fc-list then proves the picked family really contains the glyphs
		// — a family name alone resolves even when nothing covers the
		// script, so the selection is not the proof.
		secret, _ := selfSignedSecret(t)
		c := runContainer(t, runID, "fonts", baseImage, secret, "")
		waitHealthy(t, c)

		// Sample texts as codepoints for fontconfig's charset constraint:
		// 'Tiếng Việt: Đây là chữ có dấu — ă â ê ô ơ ư đ' (Vietnamese),
		// 中文 (zh), あカ日本 (ja), 한글 (ko).
		scripts := map[string]string{
			"vi": "110 111 103 e2 ea f4 1a1 1b0 1ebf",
			"zh": "4e2d 6587",
			"ja": "3042 30ab 65e5 672c",
			"ko": "d55c ad6d",
		}
		for _, family := range []string{"sans-serif", "serif", "monospace"} {
			for script, cps := range scripts {
				q := family + ":charset=" + cps
				matched := strings.TrimSpace(mustExec(t, c,
					"fc-match -f '%{family}\\n' '"+q+"' | head -1 | cut -d, -f1"))
				if matched == "" {
					t.Errorf("fc-match %q resolved to no font", q)
					continue
				}
				if out := strings.TrimSpace(mustExec(t, c,
					"fc-list '"+matched+":charset="+cps+"' family")); out == "" {
					t.Errorf("%s request for %s selected %q, which does not cover the %s codepoints (tofu)",
						family, script, matched, script)
				}
			}
		}
	})

	t.Run("ReadinessProbeIsNotAnAuthFailure", func(t *testing.T) {
		// The runtime's readiness exec probe runs every few seconds for the
		// life of the pod. An anonymous probe is an authentication failure
		// to KasmVNC: it logs "Authentication attempt failed" and, past the
		// brute_force_protection threshold, blacklists 127.0.0.1 - dropping
		// any real client that reaches KasmVNC as loopback (sidecar proxy,
		// port-forward, hostNetwork ingress). The probe must authenticate
		// with the mounted Secret instead. The password carries a quote and a
		// backslash: the probe's credential quoting must survive them.
		secret, _ := selfSignedSecret(t)
		password := `pw"with\quote and space`
		writeFile(t, filepath.Join(secret, "password"), []byte(password+"\n"))
		if err := os.Chmod(filepath.Join(secret, "password"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := runContainer(t, runID, "probe", baseImage, secret, "")
		waitHealthy(t, c)

		for i := 0; i < 20; i++ {
			if _, err := execIn(t, c, "/opt/tcdi/healthcheck.sh"); err != nil {
				t.Fatalf("probe cycle %d not ready: %v", i+1, err)
			}
		}
		// The password never shows in a process listing while probing.
		if ps := mustExec(t, c, "ps -eo args"); strings.Contains(ps, "quote") {
			t.Fatalf("probe leaked the password into argv:\n%s", ps)
		}
		log := mustExec(t, c, "cat /home/workspace/.vnc/*.log")
		for _, bad := range []string{"blacklisted", "Authentication attempt failed"} {
			if strings.Contains(log, bad) {
				t.Fatalf("20 probe cycles (plus the image HEALTHCHECK) left %q in the KasmVNC log:\n%s", bad, log)
			}
		}
		// Loopback still answers, authenticated.
		code := strings.TrimSpace(mustExec(t, c,
			`curl -sk -o /dev/null -w '%{http_code}' -u "kasm_user:$(cat /run/secrets/tcdi/password)" https://127.0.0.1:8443/`))
		if code != "200" {
			t.Fatalf("loopback after probing: got %s, want 200", code)
		}
		assertNoSecret(t, c, password)

		// Control: the marker this test looks for is real. Eight anonymous
		// requests (what the old probe sent) DO get loopback blacklisted.
		mustExec(t, c, `for i in 1 2 3 4 5 6 7 8; do curl -sk -o /dev/null https://127.0.0.1:8443/; done; true`)
		log = mustExec(t, c, "cat /home/workspace/.vnc/*.log")
		if !strings.Contains(log, "blacklisted") {
			t.Fatalf("control failed: anonymous requests did not trigger the lockout, so this test proves nothing:\n%s", log)
		}
	})

	t.Run("ProbeToleratesSecretLineEnding", func(t *testing.T) {
		// A Secret value written with a trailing line ending is the same
		// credential: the entrypoint normalizes it before kasmvncpasswd (a
		// CRLF file would otherwise store a stray '\r' the broker never
		// sends) and the probe strips one ending before logging in. The
		// boot mount carries '\r\n' — the regression case — while the
		// probe-side variants get their own mounts read via the
		// TCDI_SECRET_DIR override, so one boot covers every ending.
		secret, password := selfSignedSecret(t)
		for _, f := range []struct{ name, value string }{
			{"password", password},
			{"username", "kasm_user"},
		} {
			writeFile(t, filepath.Join(secret, f.name), []byte(f.value+"\r\n"))
			if err := os.Chmod(filepath.Join(secret, f.name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		secLF := probeSecretDir(t, "kasm_user", password, "\n")
		secEmpty := emptySecretDir(t)
		secMissing := t.TempDir()
		if err := os.Chmod(secMissing, 0o755); err != nil {
			t.Fatal(err)
		}
		c := runContainer(t, runID, "crlf", baseImage, secret, "",
			"-v", secLF+":/tmp/sec-lf:ro",
			"-v", secEmpty+":/tmp/sec-empty:ro",
			"-v", secMissing+":/tmp/sec-missing:ro")
		waitHealthy(t, c) // the image HEALTHCHECK already probed the CRLF mount

		// The normalized value is the credential: 'password' authenticates,
		// the raw CR-bearing form does not.
		code, err := httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("auth with CRLF-terminated secret: code=%d err=%v, want 200", code, err)
		}
		if code, err := httpsGet(t, c, "kasm_user", password+"\r"); err == nil &&
			code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("CR-bearing password accepted: %d", code)
		}

		// Probe passes on the '\r\n' boot mount (also via waitHealthy) and
		// on an '\n'-terminated dir; both endings are the same credential.
		if _, err := execIn(t, c, "/opt/tcdi/healthcheck.sh"); err != nil {
			t.Fatalf("probe failed on CRLF-terminated Secret mount: %v", err)
		}
		if _, err := execIn(t, c, "TCDI_SECRET_DIR=/tmp/sec-lf /opt/tcdi/healthcheck.sh"); err != nil {
			t.Fatalf("probe failed on LF-terminated Secret dir: %v", err)
		}
		// Fail closed: an empty or missing password file is NotReady, even
		// while the real endpoint is up and serving.
		for _, d := range []string{"/tmp/sec-empty", "/tmp/sec-missing"} {
			if _, err := execIn(t, c, "TCDI_SECRET_DIR="+d+" /opt/tcdi/healthcheck.sh"); err == nil {
				t.Fatalf("probe reported ready with Secret dir %s", d)
			}
		}
		assertNoSecret(t, c, password)
	})

	t.Run("DisplayDeathNotReady", func(t *testing.T) {
		secret, _ := selfSignedSecret(t)
		c := runContainer(t, runID, "death", desktopImage, secret, "")
		waitHealthy(t, c)

		// Kill the X server: the healthcheck must flip to not-ready (the
		// entrypoint exits shortly after, so accept unhealthy or dead).
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
		c := runContainer(t, runID, "browser", browserImage, secret, "")
		waitHealthy(t, c)

		// Streaming contract holds on the browser image too.
		code, err := httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("browser image endpoint auth: code=%d err=%v", code, err)
		}

		// Poll for a Chromium renderer running as uid 1000 inside a nested
		// user namespace with a seccomp filter engaged (both sandbox layers).
		initNS := strings.TrimSpace(mustExec(t, c, "readlink /proc/1/ns/user"))
		var found bool
		deadline := time.Now().Add(90 * time.Second)
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
			t.Fatalf("no Chromium renderer with engaged sandbox (nested userns + Seccomp:2, uid 1000) within 90s\nps:\n%s", ps)
		}

		// No process may carry the sandbox-disable flag. Per-arg exact match
		// so the probe's own cmdline (which quotes the flag) can't self-match.
		out := mustExec(t, c, `for p in /proc/[0-9]*/cmdline; do tr '\0' '\n' < "$p" 2>/dev/null | grep -qx -- '--no-sandbox' && echo "$p"; done; true`)
		if strings.TrimSpace(out) != "" {
			t.Fatalf("process running with --no-sandbox: %s", out)
		}
		assertNoSecret(t, c, password)
	})

	t.Run("RestartHomeAndStaleCredential", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		home := newVolume(t, runID, "home")

		c1 := runContainer(t, runID, "restart-a", desktopImage, secret, home)
		waitHealthy(t, c1)

		mustExec(t, c1, "echo persist-me > /home/workspace/marker.txt")
		mustExec(t, c1, "echo scratch > /tmp/scratch.txt")

		// Plant stale artifacts on the persistent home: a valid-format
		// kasmpasswd for a DIFFERENT password at ~/.kasmpasswd, and a user
		// config that would move the endpoint off 8443/TLS and point auth at
		// the stale file. A correct boot must ignore both.
		stale := "stale-password-000"
		mustExec(t, c1, "printf '%s\n' '"+stale+"' > /tmp/wp && rm -f /home/workspace/.kasmpasswd && "+
			"{ cat /tmp/wp; cat /tmp/wp; } | kasmvncpasswd -u kasm_user -w /home/workspace/.kasmpasswd >/dev/null && rm -f /tmp/wp")
		mustExec(t, c1, "printf 'network:\n  websocket_port: 9999\n  ssl:\n    require_ssl: false\nserver:\n  advanced:\n    kasm_password_file: /home/workspace/.kasmpasswd\n' > /home/workspace/.vnc/kasmvnc.yaml")

		dockerOK(t, "rm", "-f", c1) // rootfs (and /tmp scratch) dies here

		c2 := runContainer(t, runID, "restart-b", desktopImage, secret, home)
		waitHealthy(t, c2)

		mustExec(t, c2, "grep -q persist-me /home/workspace/marker.txt")
		out := mustExec(t, c2, "test -e /tmp/scratch.txt && echo present || echo absent")
		if strings.TrimSpace(out) != "absent" {
			t.Fatalf("rootfs scratch survived container recreation")
		}

		// The mounted Secret still authenticates on 8443 over TLS; the stale
		// password file on the volume must NOT grant access.
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
