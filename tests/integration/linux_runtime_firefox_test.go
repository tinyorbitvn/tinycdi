//go:build integration

// Contract test for the browser image's Firefox ESR fallback
// (TCDI_BROWSER=firefox, docs/images.md "Runtime contract").
//
// Firefox ESR is the measured no-node-mutation fallback for nodes without
// the Chromium Localhost seccomp+AppArmor pair, and it rides a Debian apt
// pin that jumps ESR majors (140 -> 153, FX-R15). This test pins what the
// fallback promises on whatever build the pin yields:
//
//   - the installed firefox-esr IS the Dockerfile pin;
//   - the session starts (healthy, authenticated streaming endpoint) with
//     Firefox - not Chromium - as the session browser, non-root;
//   - the seccomp-bpf layer is engaged on the browser and content
//     processes (the fallback's only sandbox layer) and nothing disables
//     it (MOZ_DISABLE_*SANDBOX, --no-sandbox);
//   - no first-run / what's-new page and no extra window (default-browser
//     or update dialog): the session store holds only the launch URL and
//     exactly one client window exists;
//   - no application updater is present (updates are Debian's apt pin);
//   - no credential leaks into logs/env.
//
// The browser image ships no Firefox enterprise policy file
// (distribution/policies.json): the Chromium managed policy is mounted by
// the kasm adapter, not baked into this image. If a Firefox policy is ever
// shipped here, add an "is honoured" subtest. (The desktop image does ship
// one - first-run/update/default-browser nags off - and
// linux_desktop_test.go asserts it is honoured there.)
//
// Run:  TCDI_IT_BUILD=1 go test -tags=integration ./tests/integration -run TestLinuxRuntimeFirefoxFallback -v

package integration

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type procEntry struct {
	pid     string
	cmdline string
}

// procTable lists every process in the container as pid + space-joined argv.
func procTable(t *testing.T, c string) []procEntry {
	t.Helper()
	out := mustExec(t, c, `for p in /proc/[0-9]*; do printf '%s\t' "${p#/proc/}"; tr '\0' ' ' < "$p/cmdline" 2>/dev/null; echo; done`)
	var procs []procEntry
	for _, line := range strings.Split(out, "\n") {
		pid, cmd, ok := strings.Cut(line, "\t")
		if !ok || pid == "" {
			continue
		}
		procs = append(procs, procEntry{pid: pid, cmdline: strings.TrimSpace(cmd)})
	}
	return procs
}

// firefoxProcs splits the process table into the session's Firefox main
// process (launched by xstartup.sh as `firefox-esr about:blank`) and its
// -contentproc children.
func firefoxProcs(procs []procEntry) (main *procEntry, content []procEntry) {
	for i, p := range procs {
		switch {
		case strings.HasPrefix(p.cmdline, "firefox-esr about:blank"):
			main = &procs[i]
		case strings.HasPrefix(p.cmdline, "/usr/lib/firefox-esr/firefox-esr -contentproc"):
			content = append(content, p)
		}
	}
	return main, content
}

func procStatus(t *testing.T, c, pid string) string {
	t.Helper()
	return mustExec(t, c, "cat /proc/"+pid+"/status 2>/dev/null || true")
}

// dockerfileArg reads a pinned `ARG <name>=<value>` from build/browser/Dockerfile.
func dockerfileArg(t *testing.T, name string) string {
	t.Helper()
	return dockerfileArgIn(t, "browser", name)
}

// dockerfileArgIn reads a pinned `ARG <name>=<value>` from build/<image>/Dockerfile.
func dockerfileArgIn(t *testing.T, image, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "build", image, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG ` + name + `=(\S+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("ARG %s not found in build/%s/Dockerfile", name, image)
	}
	return string(m[1])
}

func TestLinuxRuntimeFirefoxFallback(t *testing.T) {
	if _, err := os.Stat(seccompFile); err != nil {
		t.Fatalf("runtime seccomp profile missing: %s", seccompFile)
	}
	requireImage(t, browserImage, "build/browser")

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
	})

	secret, password := selfSignedSecret(t)
	c := runContainer(t, runID, "firefox", browserImage, secret, "", "-e", "TCDI_BROWSER=firefox")
	waitHealthy(t, c)

	t.Run("InstalledEngineIsThePin", func(t *testing.T) {
		pin := dockerfileArg(t, "FIREFOX_ESR_APT_VERSION")
		got := strings.TrimSpace(mustExec(t, c, `dpkg-query -W -f='${Version}' firefox-esr`))
		if got != pin {
			t.Fatalf("installed firefox-esr %q != Dockerfile pin %q", got, pin)
		}
		// 153.4.0esr-1~deb12u1 -> `firefox-esr --version` = "Mozilla Firefox 153.4.0esr".
		want := "Mozilla Firefox " + strings.SplitN(pin, "-", 2)[0]
		if v := strings.TrimSpace(mustExec(t, c, "firefox-esr --version 2>/dev/null")); v != want {
			t.Fatalf("firefox-esr --version = %q, want %q", v, want)
		}
	})

	t.Run("StreamingContractAndNonRoot", func(t *testing.T) {
		code, err := httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("endpoint auth with Firefox as session browser: code=%d err=%v", code, err)
		}
		if out := strings.TrimSpace(mustExec(t, c, "id -u")); out != "1000" {
			t.Fatalf("runtime uid = %s, want 1000", out)
		}
	})

	t.Run("SeccompEngagedNoSandboxDisable", func(t *testing.T) {
		// Poll for the main process plus a content process: Firefox spawns
		// content processes a moment after the window maps.
		var main *procEntry
		var content []procEntry
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			main, content = firefoxProcs(procTable(t, c))
			if main != nil && len(content) > 0 {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if main == nil || len(content) == 0 {
			ps, _ := execIn(t, c, "ps aux | head -40")
			t.Fatalf("firefox-esr main process / content processes not running within 90s (main=%v content=%d)\nps:\n%s", main, len(content), ps)
		}

		// The fallback's only sandbox layer is seccomp-bpf: the main
		// process and every content process must carry a filter, run as
		// uid 1000 with no_new_privs. (Content processes add their own
		// filter on top of the container profile.)
		for _, p := range append([]procEntry{*main}, content...) {
			st := procStatus(t, c, p.pid)
			if !strings.Contains(st, "Seccomp:\t2") {
				t.Errorf("pid %s (%.60s) has no seccomp filter:\n%s", p.pid, p.cmdline, st)
			}
			if !strings.Contains(st, "Uid:\t1000\t") {
				t.Errorf("pid %s (%.60s) is not uid 1000:\n%s", p.pid, p.cmdline, st)
			}
			if !strings.Contains(st, "NoNewPrivs:\t1") {
				t.Errorf("pid %s (%.60s) lacks no_new_privs", p.pid, p.cmdline)
			}
		}
		var contentFilters int
		for _, p := range content {
			if strings.Contains(procStatus(t, c, p.pid), "Seccomp_filters:\t2") {
				contentFilters++
			}
		}
		if contentFilters == 0 {
			t.Errorf("no content process stacked its own seccomp filter on the container profile (Seccomp_filters >= 2)")
		}

		// Nothing disables the Firefox sandbox: no MOZ_DISABLE_*SANDBOX in
		// the main process environment, no --no-sandbox anywhere, and the
		// fallback really replaced Chromium as the session browser.
		env := mustExec(t, c, "tr '\\0' '\\n' < /proc/"+main.pid+"/environ")
		if regexp.MustCompile(`(?m)^MOZ_(DISABLE_\w*SANDBOX|FORCE_DISABLE\w*)=`).MatchString(env) {
			t.Fatalf("Firefox sandbox disabled via environment:\n%s", env)
		}
		for _, p := range procTable(t, c) {
			if strings.Contains(p.cmdline, "--no-sandbox") && !strings.HasPrefix(p.cmdline, "sh ") {
				t.Fatalf("process running with --no-sandbox: %v", p)
			}
			if strings.Contains(p.cmdline, "/usr/lib/chromium/chromium") {
				t.Fatalf("TCDI_BROWSER=firefox but Chromium is running: %v", p)
			}
		}
	})

	t.Run("NoFirstRunOrUpdatePrompt", func(t *testing.T) {
		// Only the launch URL may be open: poll for the session store
		// Firefox writes a few seconds after start (interval 15s) and
		// require about:blank and no first-run/what's-new/privacy page.
		const find = `f=$(find /home/workspace -path '*sessionstore-backups/recovery.jsonlz4' 2>/dev/null | head -1); [ -n "$f" ] && grep -a -o -E 'about:[a-z]+|"url":"[^"]*"' "$f" | sort -u`
		var urls string
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			out, _ := execIn(t, c, find)
			if strings.Contains(out, "about:blank") {
				urls = out
				break
			}
			time.Sleep(5 * time.Second)
		}
		if urls == "" {
			t.Fatalf("session store never recorded the launch URL about:blank")
		}
		for _, bad := range []string{"about:welcome", "about:newtab", "whatsnew", "firstrun", "privacy-notice", "mozilla.org"} {
			if strings.Contains(urls, bad) {
				t.Fatalf("first-run/what's-new page opened (%s); session URLs:\n%s", bad, urls)
			}
		}

		// Exactly one client window: a default-browser, update or
		// profile-selection dialog would be a second top-level window.
		out := mustExec(t, c, `d=$(ls /tmp/.X11-unix | head -1); DISPLAY=":${d#X}" xprop -root _NET_CLIENT_LIST`)
		if n := strings.Count(out, "0x"); n != 1 {
			t.Fatalf("want exactly one client window, got %d: %s", n, out)
		}

		// No in-app updater: updates are the apt pin's job.
		for _, f := range []string{"/usr/lib/firefox-esr/updater", "/usr/lib/firefox-esr/updater.ini"} {
			if _, err := execIn(t, c, "test ! -e "+f); err != nil {
				t.Fatalf("Firefox ships an application updater (%s)", f)
			}
		}
	})

	assertNoSecret(t, c, password)
}
