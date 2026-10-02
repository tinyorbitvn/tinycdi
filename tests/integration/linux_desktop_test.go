//go:build integration

// Contract test for the desktop profile (tcdi/linux-desktop, V3.26): an
// XFCE4 desktop on the shared linux-base runtime.
//
// What it pins:
//
//   - the session starts under the same hardened settings as every other
//     profile (non-root, caps dropped, no-new-privileges, seccomp) AND with
//     a read-only root filesystem (only /tmp, /run/tcdi, /dev/shm and the
//     home volume writable), and the panel and the desktop windows exist;
//   - the panel's launchers point at desktop entries that exist, and the
//     terminal, the file manager and Firefox ESR open real windows from
//     them; Firefox is the same pinned build as the browser image and runs
//     with its seccomp-bpf layer on;
//   - the desktop follows a remote resize (the RandR mode switch KasmVNC's
//     resize=remote performs): panel and desktop windows take the new size;
//   - what the contract says is NOT installed is not installed: no display
//     manager, screensaver or lock, power manager, polkit agent, sudo or
//     package-manager front end, no setuid/setgid file, compositing off;
//   - a file saved in the home survives a stop/start and a recreate.
//
// Run:  TCDI_IT_BUILD=1 go test -tags=integration ./tests/integration -run TestLinuxRuntimeDesktop -v

package integration

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// roArgs runs the container with a read-only root: the pod mounts /tmp,
// /run/tcdi and /dev/shm and the home; nothing else may need to be written.
var roArgs = []string{"--read-only", "--tmpfs", "/tmp:rw,exec,size=1g,uid=1000,gid=1000"}

// sessionPrefix is a shell prefix that puts a command into the running
// desktop session's environment: its X display and D-Bus session bus.
func sessionPrefix(t *testing.T, c string) string {
	t.Helper()
	out := mustExec(t, c, `pid=$(pgrep -x xfce4-session | head -1); [ -n "$pid" ] || exit 3
tr '\0' '\n' < /proc/$pid/environ | grep -E '^(DISPLAY|DBUS_SESSION_BUS_ADDRESS|XDG_RUNTIME_DIR|XDG_CURRENT_DESKTOP|XDG_MENU_PREFIX)='`)
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "export %s='%s'; ", k, v)
	}
	return b.String()
}

// clientWindows lists the session's managed windows as "name|instance, Class"
// (quotes stripped) from the window manager's _NET_CLIENT_LIST - what
// wmctrl -l prints, using only the xprop already in the base image.
func clientWindows(t *testing.T, c string) []string {
	t.Helper()
	out, err := execIn(t, c, `for id in $(xprop -display :1 -root _NET_CLIENT_LIST | sed 's/.*# //; s/,//g'); do
  case "$id" in 0x*) ;; *) continue ;; esac
  n=$(xprop -display :1 -id "$id" _NET_WM_NAME 2>/dev/null | sed 's/^[^=]*= *//' | tr -d '"')
  k=$(xprop -display :1 -id "$id" WM_CLASS 2>/dev/null | sed 's/^[^=]*= *//' | tr -d '"')
  echo "$n|$k"
done; true`)
	if err != nil {
		t.Fatalf("listing client windows: %v", err)
	}
	var wins []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			wins = append(wins, l)
		}
	}
	return wins
}

// waitWindow polls until a managed window matching re (against "name|class",
// case-insensitive) exists, and returns it.
func waitWindow(t *testing.T, c string, re string, timeout time.Duration) string {
	t.Helper()
	rx := regexp.MustCompile("(?i)" + re)
	deadline := time.Now().Add(timeout)
	var wins []string
	for time.Now().Before(deadline) {
		wins = clientWindows(t, c)
		for _, w := range wins {
			if rx.MatchString(w) {
				return w
			}
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("no window matching %q within %s; windows: %q\nprocesses:\n%s",
		re, timeout, wins, mustExec(t, c, "ps -eo user,pid,args | cut -c1-140"))
	return ""
}

// launchEntry starts a desktop entry the way a panel launcher does: its
// Exec line, field codes dropped, inside the session's environment.
func launchEntry(t *testing.T, c, entry string) {
	t.Helper()
	prefix := sessionPrefix(t, c)
	script := prefix + `cd "$HOME"; cmd=$(grep -m1 '^Exec=' /usr/share/applications/` + entry + ` | cut -d= -f2- | sed -E 's/ ?%[a-zA-Z]//g')
[ -n "$cmd" ] || exit 4
exec $cmd`
	dockerOK(t, "exec", "-d", c, "sh", "-c", script)
}

func TestLinuxRuntimeDesktop(t *testing.T) {
	if _, err := os.Stat(seccompFile); err != nil {
		t.Fatalf("runtime seccomp profile missing: %s", seccompFile)
	}
	requireImage(t, desktopImage, "build/linux-desktop")

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
	})

	secret, password := selfSignedSecret(t)
	c := runContainer(t, runID, "desktop", desktopImage, secret, "", roArgs...)
	waitHealthy(t, c)

	t.Run("SessionStartsWithPanelAndDesktop", func(t *testing.T) {
		code, err := httpsGet(t, c, "kasm_user", password)
		if err != nil || code != http.StatusOK {
			t.Fatalf("desktop image endpoint auth: code=%d err=%v, want 200", code, err)
		}
		waitWindow(t, c, `^xfce4-panel\|`, 60*time.Second)
		waitWindow(t, c, `^Desktop\|`, 60*time.Second)

		for _, p := range []string{"xfce4-session", "xfwm4", "xfce4-panel", "xfdesktop", "xfsettingsd"} {
			out := strings.TrimSpace(mustExec(t, c, "ps -o uid= -C "+p+" | sort -u"))
			if out != "1000" {
				t.Errorf("%s not running as uid 1000 (ps uid = %q)", p, out)
			}
		}
		if out := strings.TrimSpace(mustExec(t, c, "id -u")); out != "1000" {
			t.Fatalf("runtime uid = %s, want 1000", out)
		}
		ro := dockerOK(t, "inspect", "-f", "{{.HostConfig.ReadonlyRootfs}}", c)
		if strings.TrimSpace(ro) != "true" {
			t.Fatalf("test container is not read-only: %s", ro)
		}
		assertNoSecret(t, c, password)
	})

	t.Run("CompositingOffAndNoSessionPowerSurface", func(t *testing.T) {
		prefix := sessionPrefix(t, c)
		got := strings.TrimSpace(mustExec(t, c, prefix+"xfconf-query -c xfwm4 -p /general/use_compositing"))
		if got != "false" {
			t.Fatalf("xfwm4 compositing = %q, want false", got)
		}
		// The session list is the whole desktop: no screensaver/lock client.
		for _, banned := range []string{"xfce4-screensaver", "xscreensaver", "light-locker", "xfce4-power-manager",
			"polkit", "lightdm", "gdm", "sddm", "xfce4-notifyd", "gvfsd", "ssh-agent", "gpg-agent"} {
			if out := strings.TrimSpace(mustExec(t, c, "ps -eo args | grep -i -- '"+banned+"' | grep -v grep || true")); out != "" {
				t.Errorf("unexpected process %q running in the desktop session: %s", banned, out)
			}
		}
	})

	t.Run("NotInstalled", func(t *testing.T) {
		// Packages and binaries the contract forbids. libpolkit-gobject
		// (a library xfce4-session links) is not a polkit agent.
		pkgs := mustExec(t, c, `dpkg-query -W -f='${Package}\n' | sort`)
		banned := regexp.MustCompile(`^(sudo|lightdm.*|gdm3|sddm|xdm|xfce4-screensaver|xscreensaver.*|light-locker.*|xfce4-power-manager.*|` +
			`policykit-1.*|polkitd.*|pkexec|.*polkit.*agent.*|.*polkit-gnome.*|synaptic|gnome-software.*|packagekit.*|` +
			`software-properties.*|update-manager.*|apt-xapian-index|gdebi.*|xfce4-notifyd|gvfs.*|udisks2|upower|systemd-logind|libpam-systemd)$`)
		for _, p := range strings.Fields(pkgs) {
			if banned.MatchString(p) {
				t.Errorf("forbidden package installed in the desktop image: %s", p)
			}
		}
		for _, bin := range []string{"sudo", "su-to-root", "pkexec", "xflock4", "xfce4-session-logout", "xfsm-shutdown-helper",
			"shutdown", "reboot", "poweroff", "systemctl", "loginctl"} {
			if out := strings.TrimSpace(mustExec(t, c, "command -v "+bin+" || true")); out != "" {
				t.Errorf("forbidden binary %q present at %s", bin, out)
			}
		}
		if out := strings.TrimSpace(mustExec(t, c, `find / -xdev -type f \( -perm -4000 -o -perm -2000 \) 2>/dev/null; true`)); out != "" {
			t.Errorf("setuid/setgid files in the desktop image:\n%s", out)
		}
		// The shortcuts that reach log-out/lock are gone from the defaults.
		if out := strings.TrimSpace(mustExec(t, c, `grep -rE 'xfce4-session-logout|xflock4' /etc/xdg/xfce4 || true`)); out != "" {
			t.Errorf("log-out/lock still wired in the XFCE defaults:\n%s", out)
		}
		// What the contract promises IS installed.
		for _, bin := range []string{"xfce4-session", "xfwm4", "xfce4-panel", "xfdesktop", "xfce4-settings-manager", "thunar",
			"xfce4-terminal", "mousepad", "xarchiver", "ristretto", "firefox-esr"} {
			if out := strings.TrimSpace(mustExec(t, c, "command -v "+bin+" || true")); out == "" {
				t.Errorf("%s is not installed in the desktop image", bin)
			}
		}
	})

	t.Run("PanelLaunchersOpenTerminalFileManagerAndFirefox", func(t *testing.T) {
		panel := mustExec(t, c, "cat /etc/xdg/xfce4/panel/default.xml")
		if n := strings.Count(panel, `name="panel-`); n != 1 {
			t.Fatalf("panel config must define exactly one panel, found %d", n)
		}
		entries := map[string]bool{}
		for _, m := range regexp.MustCompile(`value="([^"]+\.desktop)"`).FindAllStringSubmatch(panel, -1) {
			entries[m[1]] = true
		}
		for _, want := range []string{"xfce4-terminal.desktop", "thunar.desktop", "firefox-esr.desktop"} {
			if !entries[want] {
				t.Fatalf("panel has no launcher for %s (launchers: %v)", want, entries)
			}
			mustExec(t, c, "test -r /usr/share/applications/"+want)
		}

		launchEntry(t, c, "xfce4-terminal.desktop")
		waitWindow(t, c, `\|xfce4-terminal, `, 45*time.Second)
		launchEntry(t, c, "thunar.desktop")
		waitWindow(t, c, `\|Thunar, `, 45*time.Second)
	})

	t.Run("FirefoxIsThePinnedBuildAndSandboxed", func(t *testing.T) {
		pin := dockerfileArgIn(t, "linux-desktop", "FIREFOX_ESR_APT_VERSION")
		if browserPin := dockerfileArg(t, "FIREFOX_ESR_APT_VERSION"); pin != browserPin {
			t.Fatalf("desktop firefox-esr pin %q != browser image pin %q (one pinned source)", pin, browserPin)
		}
		got := strings.TrimSpace(mustExec(t, c, `dpkg-query -W -f='${Version}' firefox-esr`))
		if got != pin {
			t.Fatalf("installed firefox-esr %q != Dockerfile pin %q", got, pin)
		}

		launchEntry(t, c, "firefox-esr.desktop")
		waitWindow(t, c, `\|(Navigator|firefox-esr|firefox), `, 90*time.Second)

		// seccomp-bpf (the only Firefox layer without node profiles) is
		// engaged on the content processes, and nothing disables it.
		var found bool
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) && !found {
			out := mustExec(t, c, `for p in /proc/[0-9]*; do
  if tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q -- '-contentproc'; then
    printf '%s %s\n' "${p#/proc/}" "$(grep '^Seccomp:' $p/status | tr -d '\t')"
  fi
done; true`)
			for _, l := range strings.Split(out, "\n") {
				if strings.HasSuffix(strings.TrimSpace(l), "Seccomp:2") {
					found = true
				}
			}
			if !found {
				time.Sleep(2 * time.Second)
			}
		}
		if !found {
			t.Fatalf("no Firefox content process with seccomp-bpf engaged:\n%s", mustExec(t, c, "ps -eo args | grep -i firefox | head"))
		}
		if out := strings.TrimSpace(mustExec(t, c, `for p in /proc/[0-9]*/environ; do tr '\0' '\n' < "$p" 2>/dev/null | grep -E '^MOZ_DISABLE_[A-Z_]*SANDBOX' && echo "$p"; done; true`)); out != "" {
			t.Fatalf("a Firefox sandbox is disabled by environment: %s", out)
		}

		// The shipped policy is honoured: no default-browser or first-run
		// dialog opens next to the browser window.
		time.Sleep(5 * time.Second)
		ffWindows := 0
		for _, w := range clientWindows(t, c) {
			if regexp.MustCompile(`(?i)\|(Navigator|firefox-esr|firefox), `).MatchString(w) {
				ffWindows++
			}
		}
		if ffWindows != 1 {
			t.Fatalf("expected exactly one Firefox window (no first-run/default-browser dialog), found %d: %q", ffWindows, clientWindows(t, c))
		}
		mustExec(t, c, "test -s /usr/lib/firefox-esr/distribution/policies.json")
	})

	t.Run("FollowsRemoteResize", func(t *testing.T) {
		// resize=remote makes the KasmVNC client switch the X output to the
		// browser viewport size over RandR; the desktop must follow.
		mustExec(t, c, "xrandr -d :1 --output VNC-0 --mode 1920x1080")
		deadline := time.Now().Add(30 * time.Second)
		var geo string
		for time.Now().Before(deadline) {
			geo = mustExec(t, c, `xwininfo -display :1 -root -tree | grep -E '"(xfce4-panel|Desktop)":' | grep -E '[0-9]{3,}x[0-9]+\+' || true`)
			if strings.Contains(geo, `"xfce4-panel"`) && strings.Contains(geo, `"Desktop"`) &&
				strings.Contains(geo, "1920x37+") && strings.Contains(geo, "1920x1080+0+0") {
				break
			}
			time.Sleep(1 * time.Second)
		}
		if !strings.Contains(geo, "1920x37+") || !strings.Contains(geo, "1920x1080+0+0") {
			t.Fatalf("panel/desktop did not follow the 1920x1080 mode switch:\n%s", geo)
		}
		mustExec(t, c, "xrandr -d :1 --output VNC-0 --mode 1280x800")
	})

	t.Run("HomeSurvivesStopStartAndRecreate", func(t *testing.T) {
		home := newVolume(t, runID, "desktop-home")
		c1 := runContainer(t, runID, "desktop-home-a", desktopImage, secret, home, roArgs...)
		waitHealthy(t, c1)
		waitWindow(t, c1, `^xfce4-panel\|`, 60*time.Second)

		// A user file, and a per-user XFCE setting the desktop writes itself.
		mustExec(t, c1, "mkdir -p /home/workspace/Documents && echo saved-in-home > /home/workspace/Documents/notes.txt")
		mustExec(t, c1, "test -d /home/workspace/.config/xfce4")

		dockerOK(t, "stop", c1)
		dockerOK(t, "start", c1)
		waitHealthy(t, c1)
		waitWindow(t, c1, `^xfce4-panel\|`, 60*time.Second)
		waitWindow(t, c1, `^Desktop\|`, 60*time.Second)
		if got := strings.TrimSpace(mustExec(t, c1, "cat /home/workspace/Documents/notes.txt")); got != "saved-in-home" {
			t.Fatalf("file lost across stop/start: %q", got)
		}

		dockerOK(t, "rm", "-f", c1)
		c2 := runContainer(t, runID, "desktop-home-b", desktopImage, secret, home, roArgs...)
		waitHealthy(t, c2)
		waitWindow(t, c2, `^xfce4-panel\|`, 60*time.Second)
		waitWindow(t, c2, `^Desktop\|`, 60*time.Second)
		if got := strings.TrimSpace(mustExec(t, c2, "cat /home/workspace/Documents/notes.txt")); got != "saved-in-home" {
			t.Fatalf("file lost across recreate: %q", got)
		}
	})
}

// TestLinuxRuntimeBrowserBaseline pins the V3.26 promise that splitting the
// base out of the old linux-desktop image did not grow the browser image:
// package count, newly-introduced packages and image size stay within 2 % of
// the pre-split image recorded in testdata/browser-image-baseline.json.
//
// When chromium/firefox-esr repins legitimately move these numbers, refresh
// the baseline in that PR; never to absorb added profile software.
func TestLinuxRuntimeBrowserBaseline(t *testing.T) {
	requireImage(t, browserImage, "build/browser")

	raw, err := os.ReadFile(repoRoot + "/tests/integration/testdata/browser-image-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	var base struct {
		SizeBytes int64    `json:"sizeBytes"`
		Packages  []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(base.Packages) == 0 || base.SizeBytes == 0 {
		t.Fatal("baseline is empty")
	}
	inBase := map[string]bool{}
	for _, p := range base.Packages {
		inBase[p] = true
	}

	out := dockerOK(t, "run", "--rm", "--entrypoint", "dpkg-query", browserImage, "-W", "-f", "${Package}\n")
	var now, added []string
	for _, p := range strings.Fields(out) {
		now = append(now, p)
		if !inBase[p] {
			added = append(added, p)
		}
	}
	sizeOut := strings.TrimSpace(dockerOK(t, "image", "inspect", "-f", "{{.Size}}", browserImage))
	var size int64
	if _, err := fmt.Sscan(sizeOut, &size); err != nil {
		t.Fatalf("image size %q: %v", sizeOut, err)
	}

	maxPkgs := len(base.Packages) + len(base.Packages)*2/100
	maxSize := base.SizeBytes + base.SizeBytes*2/100
	t.Logf("browser packages %d (baseline %d, limit %d; new names: %v); size %d (baseline %d, limit %d)",
		len(now), len(base.Packages), maxPkgs, added, size, base.SizeBytes, maxSize)
	if len(now) > maxPkgs {
		t.Errorf("browser image package count %d exceeds baseline %d by more than 2%%; new: %v", len(now), len(base.Packages), added)
	}
	if len(added) > len(base.Packages)*2/100 {
		t.Errorf("browser image gained %d package names beyond 2%% of the baseline: %v", len(added), added)
	}
	if size > maxSize {
		t.Errorf("browser image size %d exceeds baseline %d by more than 2%%", size, base.SizeBytes)
	}
}
