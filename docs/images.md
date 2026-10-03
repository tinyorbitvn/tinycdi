# Images — build, run, test

TinyCDI ships seven images, all built from `build/<name>/Dockerfile` with the
repository root as build context. `build/release-images.txt` is the released
set; CI (`images.yml`, `release.yml`) is the only path that pushes — by
digest, scanned, then signed before any tag exists (see `.github/README.md`).
Local builds are local-only — do NOT push to any registry.
For running unmodified `kasmweb/*` images behind the injected adapter
(`spec.linux.adapter: kasm`, `build/kasm-adapter/`), see
`docs/kasm-images.md`.

## Platform images

Three components (`docs/adr/0005-backend-frontend-operator.md`):

| Image | Source | Listeners | Purpose |
|---|---|---|---|
| `tinycdi-backend` | `cmd/backend` (distroless, static Go) | `:8443` app (public API `/v1/*`, portal host) · `:8444` session (launch, desktop proxy, websockify — session host) · `:9443` internal mTLS (operator broker API) · `:9090` metrics | public API + session gateway in one binary/deployment |
| `tinycdi-frontend` | `build/frontend` + the `web/` SPA built in-image (distroless) | `:8443` HTTPS | static SPA server + security headers; no API proxy |
| `tinycdi-operator` | `cmd/operator` (distroless, static Go) | metrics/health | reconciles `Workspace` CRDs into runtime pods |

Routing: on the portal host `/v1/` goes to backend `:8443` and everything
else to frontend `:8443`; the session host goes to backend `:8444`.
Sessions render inside the portal through an iframe on the isolated
session origin — runtime content is never served from the portal origin.

```sh
make docker-build-images                 # every release image, tcdi/<name>:local
make docker-build-backend                # one image (docker-build-<name>)
make docker-build-images IMAGE_TAG=dev   # IMAGE_PREFIX / IMAGE_TAG override tags
```

The release additionally attaches static `tinycdi-backend` and
`tinycdi-operator` linux/amd64 binaries.

## Linux runtime images

One base image and two profiles built `FROM` it:

```
debian:bookworm-slim ─► tcdi/linux-base ─┬─► tcdi/linux-desktop   (XFCE4 desktop + Firefox ESR)
                                          └─► tcdi/browser         (openbox kiosk + Chromium + Firefox ESR)
```

| Image | Contents | Purpose |
|---|---|---|
| `tcdi/linux-base` | `debian:bookworm-slim` (digest-pinned) + KasmVNC 1.5.0 (sha256-verified deb), X utilities, D-Bus, fontconfig + DejaVu/Noto/WenQuanYi fonts (see *Budgets* for the kept faces), the entrypoint/healthcheck, the system KasmVNC policy, the `workspace` user (uid 1000), the setuid/setgid strip. **No window manager, no terminal, no browser** | the runtime contract (below) and nothing a profile decides; the starting point for custom images |
| `tcdi/linux-desktop` | `FROM tcdi/linux-base` + XFCE4 (minimal set) + Firefox ESR — see [The desktop profile](#the-desktop-profile-tcdilinux-desktop) | `experience: Desktop` templates |
| `tcdi/browser` | `FROM tcdi/linux-base` + openbox + xterm + `chromium` + `chromium-sandbox` + `firefox-esr` (apt-pinned) | `experience: Browser` templates — the session launches the browser maximized |

Splitting the base out changed what is *inherited*, not what ships:
`tcdi/browser` keeps exactly the pre-split package list (openbox and xterm moved
from the old desktop layer into the browser Dockerfile) and
`TestLinuxRuntimeBrowserBaseline` fails if its package count or size grows by
more than 2 % (`tests/integration/testdata/browser-image-baseline.json`).

Pinned inputs (`docs/compatibility.md`):

- base: `debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251`
- KasmVNC: `kasmvncserver_bookworm_1.5.0_amd64.deb`, sha256 `770fd3df51510beecc89666879d82faf411276e68c6e11df612f736b891b5f71`
- Chromium: `154.0.8037.92-1~deb12u1` (apt pin — repin on Debian security updates)
- Firefox ESR (fallback): `153.4.0esr-1~deb12u1` (apt pin; the same pin in `tcdi/browser`, where it is the fallback browser, and in `tcdi/linux-desktop`, where it is the desktop's browser — one pinned source, bumped together)

## Published runtime images (release train)

Runtime images ship on their own release train (`.github/workflows/
runtime-images.yml`), decoupled from control-plane `v*.*.*` releases:
every main push touching `build/linux-base/**`, `build/linux-desktop/**` or
`build/browser/**`, weekly, and on dispatch. The base builds first; the two
profiles build `FROM` its exact pushed digest. Signed digests are promoted to
`ghcr.io/tinyorbitvn/tinycdi-{linux-base,linux-desktop,browser}:rt-YYYYMMDD.N` and
the manifest `runtime-images.json` (three images; the profiles also record the
engine versions they carry) is attached to the GitHub Release
`runtime-YYYY.MM.DD` (see `.github/README.md` for the manifest shape and
the cosign verify line). Deployments pin `images.*.digest`/`builtAt`
from that manifest — values are GitOps-owned and never written back by
the train; a new desktop image reaches a deployment by bumping
`images.linuxDesktop` (and the base digest, if it keeps one) in its values.
`runtime-freshness.yml` keeps the chromium and firefox-esr pins current
(daily check + auto PR; the desktop's Firefox pin is checked against the
browser's and bumped with it) and fails when the newest `runtime-*` release is
older than 14 days (`docs/security/vulnerability-policy.md` §5).

## Runtime contract

- HTTPS streaming endpoint on container port **8443** (websocket transport
  only — KasmVNC 1.5.0 has no HTTP tunnel fallback). Auth = HTTP Basic against
  a kasmpasswd file holding one write-only (`w`, non-owner) user; owner-gated
  `/api/*` management routes therefore deny every request.
- `HEALTHCHECK` (`/opt/tcdi/healthcheck.sh`) succeeds only while **both** the
  X display (`xdpyinfo -display :1`) and the endpoint answer. If the display
  dies the container goes unhealthy, then exits for restart semantics. The
  probe logs in with the mounted Secret (read from `/run/secrets/tcdi`, passed
  to `curl` on stdin — never in argv, never printed): an anonymous probe would
  count as an authentication failure to KasmVNC and trip its
  brute-force protection, blacklisting `127.0.0.1` and dropping any real
  client that arrives as loopback (sidecar proxy, port-forward, hostNetwork
  ingress). `TestLinuxRuntimeReadinessAndHome/ReadinessProbeIsNotAnAuthFailure`
  runs 20 probe cycles and asserts no `blacklisted` / `Authentication attempt
  failed` line in the KasmVNC log and that loopback still answers 200; the
  kasm adapter's healthcheck has the same shape and the same test.
- Credentials come from a **mounted Secret as files** under
  `/run/secrets/tcdi/` — never env, never logged:

  | file | required | use |
  |---|---|---|
  | `password` | yes | KasmVNC Basic-auth password |
  | `username` | no | default `kasm_user` |
  | `tls.crt` / `tls.key` | yes | endpoint TLS material |

  One trailing line ending (`\n` or `\r\n`) on `password`/`username` is
  ignored — the value itself is used verbatim; an empty `password` is
  rejected.

- Ephemeral credential/config material lives on a writable `/run/tcdi`
  mount (tmpfs/emptyDir), **outside** the persistent home.
- Home is `/home/workspace` (uid 1000, persistent mount point). On every
  boot the entrypoint force-rewrites `~/.kasmpasswd` (symlink →
  `/run/tcdi/kasmpasswd`) and `~/.vnc/kasmvnc.yaml`, so a stale credential
  or config file on the volume can never override the mounted Secret or the
  system policy `/etc/kasmvnc/kasmvnc.yaml`. `-xstartup` is passed
  explicitly, so a stale `~/.vnc/xstartup` cannot hijack the session either.
- `/dev/shm` must be a bounded mount: Docker `--shm-size 256m`; Kubernetes
  `emptyDir{medium: Memory, sizeLimit: 256Mi}`. Chromium crashes/renders
  corrupt on the 64 MiB default.
- Runs as non-root uid 1000 (`USER 1000:1000`) with `--cap-drop ALL` +
  `no-new-privileges`. The image strips every setuid/setgid bit — nothing
  in the runtime needs one — and ships **no** TLS key material: the
  Debian `ssl-cert` snakeoil pair (`/etc/ssl/certs/ssl-cert-snakeoil.pem`,
  `/etc/ssl/private/ssl-cert-snakeoil.key`) is deleted at build; the only
  cert KasmVNC serves is the mounted Secret staged to `/run/tcdi/`
  (`network.ssl.pem_certificate`/`pem_key` in `/etc/kasmvnc/kasmvnc.yaml`
  point there, so the runtime never falls back to the absent snakeoil
  pair). The browser image keeps `chrome-sandbox` setuid — it is
  Chromium's sandbox helper.
- **Chromium sandbox prerequisite:** Chromium needs unprivileged user
  namespaces. On Kubernetes nodes install the locked Localhost seccomp +
  AppArmor pair (`docs/compatibility.md`, "Node security profile") before
  the browser profile ships. For local Docker use the resolved profile:
  `--security-opt seccomp=tests/integration/testdata/seccomp-runtime.json`.
  `firefox-esr` is the measured fallback (`TCDI_BROWSER=firefox`) with
  reduced isolation (seccomp-bpf only, no userns layer).
- Pod placement is set by the template's typed `spec.placement` fields
  (`nodeSelector`, `tolerations`, `runtimeClassName`) and `spec.linux.hostUsers`.
  The `workspaces.cdi.tinyorbit.vn/node-selector` template annotation is
  **deprecated**: it is honored for one release and loses to
  `spec.placement.nodeSelector` when both are set.

## The desktop profile (`tcdi/linux-desktop`)

An XFCE4 desktop for `experience: Desktop` templates, built `FROM
tcdi/linux-base` (`build/linux-desktop/`). It starts under its own D-Bus
session bus (`xstartup.sh` → `dbus-launch xfce4-session`) and shows a panel at
the bottom of the screen with the Applications menu, launchers for the
terminal, the file manager and Firefox, the window list and a clock, over a
plain `#274060` desktop with a *Home* icon.

### Installed

| Role | Package | Why this one |
|---|---|---|
| Session | `xfce4-session` | starts the four components below from a fixed list; saves nothing on exit |
| Window manager | `xfwm4` | compositing **off** (no GPU in the session; a software compositor only adds frames for KasmVNC to encode) |
| Panel | `xfce4-panel` | one panel: applications menu, three launchers, window list, clock |
| Desktop | `xfdesktop4` | backdrop and a *Home* icon |
| Settings | `xfce4-settings` (+ `xfconf`, `xfsettingsd`) | the Settings Manager, theme and keyboard/mouse settings; its package pulls in `python3` (a dependency of `xfce4-helpers`) |
| File manager | `thunar`, `thunar-archive-plugin` | the XFCE file manager, with *Extract here* / *Create archive* |
| Terminal | `xfce4-terminal` | |
| Text editor | `mousepad` | |
| Archive tool | `xarchiver` | |
| Image viewer | `ristretto` | **PDFs open in Firefox's built-in viewer** (set in `/etc/xdg/mimeapps.list`): a separate PDF reader (`atril`, `evince`) drags in WebKitGTK, GStreamer and poppler — ~100 more packages — for a viewer Firefox already provides |
| Browser | `firefox-esr` | the same apt pin as `tcdi/browser` (see *Pinned inputs*); a panel launcher; a policy file (`distribution/policies.json`) switches off the updater, the default-browser prompt, the first-run/post-update pages, telemetry and the sponsored/Pocket tiles |
| Look | `greybird-gtk-theme`, `adwaita-icon-theme`, DejaVu Sans / Sans Mono, Noto Sans / Serif, WenQuanYi Micro Hei | one readable light theme; the font set covers Latin/Greek/Cyrillic (incl. Vietnamese) and zh/ja/ko |
| Glue | `xdg-utils` | `xdg-open` for links and *Open with* |

### Deliberately not installed (and, where the package ships them, removed)

| Not present | Why |
|---|---|
| display manager (`lightdm`, `gdm`, `sddm`) | KasmVNC starts the session; there is no login screen to reach |
| screensaver / screen lock (`xscreensaver`, `xfce4-screensaver`, `light-locker`, `xflock4`, the *Lock Screen* shortcut) | a VNC framebuffer never blanks, and a lock would lock the user out of their own session; the screensaver autostart `xfce4-session` ships is deleted |
| power management (`xfce4-power-manager`, `upower`), log-out / shutdown / suspend actions | there is no logind, polkit or sudo to act on — the buttons would be dead; logging out would end the session and restart the container. The panel has no *actions* plugin, the menus have no log-out entry, `Ctrl+Alt+Del` is unbound, and `xfce4-session-logout` / `xfsm-shutdown-helper` are deleted |
| polkit agent, `pkexec` | privilege escalation has no place in a non-root `no-new-privileges` container (`libpolkit-gobject` is only a library `xfce4-session` links) |
| `sudo`, any setuid/setgid file | the image strips every setuid/setgid bit (asserted by `TestLinuxRuntimeDesktop`) |
| package-manager front ends (`synaptic`, `gnome-software`, `packagekit`) | packages are baked into the image (pinned, scanned, signed); installing at runtime is not a supported path |
| `gvfs`, `udisks2`, notification daemon, audio | no removable media or trash integration, no desktop notifications, no sound — each is a daemon plus a dependency tree for a feature a stream-only workspace cannot use. Thunar deletes files permanently (it asks first) |
| the Orbit brand wallpaper | the Orbit artwork and marks are TinyOrbit trademarks, not MIT-licensed (`TRADEMARKS.md`), so the image ships a plain colour; override the backdrop in your own derived image |

### Where things live

- The home is `/home/workspace` on the data volume: XFCE/Thunar/Firefox
  settings and the user's files persist with it. System defaults (panel,
  theme, `xfwm4`) live under `/etc/xdg/xfce4` and apply until a user changes
  them.
- The session's runtime dir (`XDG_RUNTIME_DIR`) is `/run/tcdi/xdg`, on the
  ephemeral credential mount. With `/tmp`, `/run/tcdi`, `/dev/shm` and the
  home writable the desktop also runs on a **read-only root filesystem**
  (`TestLinuxRuntimeDesktop` runs it that way).
- The desktop follows a remote resize: `resize=remote` (the KasmVNC client
  switching the X output to the viewport size over RandR) re-lays the panel
  and the backdrop at the new size; nothing in the image fixes a resolution.
- **Firefox in the desktop is not equivalent to the Chromium browser
  template for untrusted browsing.** It runs under the RuntimeDefault seccomp
  profile (seccomp-bpf on its content processes) *without* the user-namespace
  sandbox layer — the same reduced isolation the browser image's Firefox
  fallback documents (`TCDI_BROWSER=firefox`). The Chromium template's nested
  user-namespace sandbox needs the node Localhost seccomp+AppArmor pair and
  the browser image; use that template for untrusted web content, and treat
  the desktop's Firefox as a convenience inside an already-isolated
  workspace. It needs the bounded `/dev/shm` of the runtime contract.

### Posture (unchanged from the base)

Non-root uid 1000, `--cap-drop ALL`, `no-new-privileges`, the runtime
seccomp profile (RuntimeDefault, or the node Localhost profile where the
template selects one), whatever AppArmor posture the deployment applies to the
pod (the image depends on none, so any pod-level setting works unchanged),
home on the data volume, and the endpoint/credential contract above. The
desktop adds no listener, no capability and no setuid/setgid file.

### Upgrading an existing desktop workspace

A workspace moves to this image when its template's `spec.linux.image`
digest changes; the pod is recreated, so the running session restarts
(open windows are lost, as with any restart) and the **retained home is
reused as it is**. What an old openbox/xterm session left in that home
(listed from a real old-image run): `.Xauthority`, `.bashrc`/`.profile`/
`.bash_logout`, `.cache/fontconfig`, `.cache/openbox/`, `.vnc/*` (a stale pid
file, `kasmvnc.yaml`, `passwd`) and the `.kasmpasswd` symlink — there was no
`~/.config` at all. On the first XFCE start:

- XFCE creates `~/.config/xfce4` and `~/.config/Thunar`, `~/.cache/sessions`,
  `~/.local`, `~/.dbus` and `~/Desktop`; it reads the system defaults from
  `/etc/xdg/xfce4` until the user changes a setting. Nothing that was in the
  home is modified or deleted (asserted file-by-file).
- The openbox leftovers (`~/.cache/openbox`, a user's own
  `~/.config/openbox/rc.xml`) are ignored, not removed. `~/.Xresources` was
  read by xterm only; the XFCE session does not load it.
- A stale `~/.vnc/xstartup`, `~/.vnc/kasmvnc.yaml` or credential file cannot
  take over: the entrypoint passes the session explicitly and rewrites the
  config and the credential link on every boot (runtime contract).
- The stale `.vnc/<host>:1.pid` and `.Xauthority` are replaced by KasmVNC.

`TestLinuxRuntimeDesktop/UpgradeFromOpenboxHome` seeds a volume with exactly
that state plus user files and stale hijack attempts, boots the desktop on it
(read-only root), and checks the panel and desktop come up, no `xterm` takes
over, every pre-existing file is byte-identical, and a second stop/start works.

### Budgets (V3.26, measured 2026-10-03, trivy 0.70.0, Docker 29.8, 8-core host)

| | old `linux-desktop` (openbox + xterm) | new `linux-desktop` (XFCE4 + Firefox ESR) | `linux-base` (new) | `browser` before → after |
|---|---|---|---|---|
| Image size (uncompressed / compressed) | 790 MiB / 199 MB | 1202 MiB / 289 MB | 598 MiB / 141 MB | 1.726 GiB → 1.783 GiB (+0.90 %) |
| Packages (dpkg) | 328 | 383 | 252 | 373 → 375 (added `fonts-noto-core`, `fonts-wqy-microhei`) |
| trivy HIGH/CRITICAL **with a fix** (the gate) | 0 | 0 | 0 | 0 → 0 |
| trivy HIGH/CRITICAL total (CRITICAL + HIGH, unfixed in bookworm) | 17 + 150 | 16 + 140 | 14 + 94 | 17 + 153 → 17 + 153 |
| Idle CPU of a fresh session (mean over 60 s, after 45 s) | 14 mCPU | 14 mCPU | — | unchanged |
| Idle memory (working set) | 37 MiB | 98 MiB | — | unchanged |
| Time to first frame (container start → panel/`xterm` window mapped) | 2.4 s | 2.3 s | — | unchanged |
| Time to healthy (the 5 s healthcheck interval dominates) | 5.4 s | 5.4 s | — | unchanged |

**Fonts (added 2026-10-03; the size and package rows above include them,
the trivy rows predate them — the runtime-train scan re-measures on
publish).** `linux-base` ships `fontconfig`, `fonts-dejavu-core`,
`fonts-noto-core` trimmed to the eight Latin/Greek/Cyrillic faces
(Sans and Serif × Regular/Bold/Italic — Vietnamese included; the
package's other ~260 script faces are deleted) and `fonts-wqy-microhei`
for CJK (zh/ja/ko). The CJK choice is a budget decision: image size in
this table (and in `TestLinuxRuntimeBrowserBaseline`) is
`docker image inspect .Size`, which on the containerd store counts
≈1.8× the bytes of added files. Measured against the browser's +2 %
headroom (~35 MiB over the pre-split baseline): full `fonts-noto-cjk`
adds ~89 MiB unpacked (~160 MiB on the metric) — far over; the
`NotoSansCJK-Regular.ttc` face alone adds ~35 MiB (+2.39 % measured —
just over); `fonts-droid-fallback` has no Korean; `fonts-wqy-microhei`
adds ~8 MiB and covers zh/ja/ko (CN-flavoured glyph forms for ja/ko —
correct coverage, not native typography). Proper Noto CJK therefore
needs a baseline re-issue by decision; the baseline cannot absorb added
software under its own rule.

Under use (measured on the new desktop): opening a terminal, Thunar and
Firefox (three tabs) bursts to ~2 CPU for ~40 s while Firefox starts, then
settles at 20–40 mCPU and a **555 MiB working set** (peak 888 MiB including
page cache). Scripted mouse and keyboard input into a terminal for five
minutes — what the soak harness does — stays at p50 23 / p95 52 / max 61
mCPU and 111–112 MiB.

Template sizing (pod requests = limits): the desktop template stays at
**1 CPU / 2 GiB / 5 GiB** (`deploy/helm/tinycdi/ci/example-values.yaml`) —
2 GiB holds Firefox with several real pages next to the 256 MiB `/dev/shm`,
and 1 CPU is where Firefox's start-up burst is merely slower, not starved.
A desktop used only for terminal/file work fits 500 mCPU / 1 GiB. The **soak
profile** (sessions that are only driven with synthetic input, no browser
launched) gets its own number: **250 mCPU / 512 MiB / 1 GiB** — 4× the
measured memory and 4× the p95 CPU. See `docs/runbooks/capacity.md`.

## Build a custom desktop image on the base

`tcdi/linux-base` is the runtime contract with nothing on top: build your own
desktop (another environment, your applications, your branding) by extending
it. Take the digest from the `runtime-images.json` of a `runtime-*` release
(or `images.linuxBase.digest` of a released chart) and verify it with the
cosign line in `.github/README.md`.

```dockerfile
FROM ghcr.io/tinyorbitvn/tinycdi-linux-base@sha256:<digest>
USER root
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends <your window manager / desktop / apps> \
 && rm -rf /var/lib/apt/lists/*
COPY xstartup.sh /opt/tcdi/xstartup.sh
RUN chmod 755 /opt/tcdi/xstartup.sh \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} +
USER 1000:1000
```

What the base fixes, and what a derived image must keep:

- **The session is `/opt/tcdi/xstartup.sh`.** `kasmvncserver` runs it on display
  `:1` as uid 1000 with `HOME=/home/workspace`; the session — and the
  container — ends when it exits, so it must `exec` the desktop (or wait on
  it). Replace the file; do not change the entrypoint.
- **Leave `ENTRYPOINT`, `HEALTHCHECK` and the `USER` alone.** The operator
  depends on `/opt/tcdi/entrypoint.sh` (secrets as files, TLS and kasmpasswd
  staged on `/run/tcdi`, endpoint on 8443) and `/opt/tcdi/healthcheck.sh`.
- **Write only to** `$HOME`, `/tmp`, `/run/tcdi` and `/dev/shm`; point
  `XDG_RUNTIME_DIR` at a directory under `/run/tcdi` (as the desktop profile
  does) so the image also runs on a read-only root.
- **Stay non-root and setuid-free.** Do not install `sudo` or a polkit agent;
  strip setuid/setgid bits after your `apt-get` (the `find` above).
  Compositing off and no screensaver, lock or power actions are what make a
  VNC session behave; keep a log-out button out of reach.
- **Pin and rebuild.** Pin your packages like the shipped profiles do, rebuild
  when the base digest changes, and scan the result with the same gate
  (`trivy image --severity CRITICAL,HIGH --ignore-unfixed`). The base is on the
  runtime train: a stale base is a stale image.
- **Check it.** Point the contract tests at your image:
  `TCDI_IT_DESKTOP_IMAGE=<your image> go test -tags=integration
  ./tests/integration -run TestLinuxRuntimeReadinessAndHome`; copy
  `TestLinuxRuntimeDesktop` for window-level checks.
- Reference it from a template as a digest-pinned `spec.linux.image`
  (`<repo>@sha256:<64hex>` — the CRD rejects tags).

## Build

`make docker-build-images` builds every image in order (`linux-base` first);
`make docker-build-linux-desktop` / `docker-build-browser` build one profile
with the base. By hand:

```sh
docker build -f build/linux-base/Dockerfile -t tcdi/linux-base:local .
docker build -f build/linux-desktop/Dockerfile -t tcdi/linux-desktop:local \
    --build-arg BASE_IMAGE=tcdi/linux-base:local .
docker build -f build/browser/Dockerfile -t tcdi/browser:local \
    --build-arg BASE_IMAGE=tcdi/linux-base:local .
```

## Run (local, hardened)

```sh
# Secret material lives OUTSIDE the working tree (gitignore'd scratch dir),
# never in ./secret/ inside the checkout.
SECRETS_DIR="$(mktemp -d /tmp/tcdi-secrets.XXXXXX)"
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$SECRETS_DIR/tls.key" -out "$SECRETS_DIR/tls.crt" \
  -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost,IP:127.0.0.1
head -c 24 /dev/urandom | base64 > "$SECRETS_DIR/password"
printf 'kasm_user\n' > "$SECRETS_DIR/username"
docker volume create ws-home
docker run -d --name ws -p 127.0.0.1:0:8443 \
  -v "$SECRETS_DIR:/run/secrets/tcdi:ro" \
  -v ws-home:/home/workspace \
  --tmpfs /run/tcdi:rw,exec,uid=1000,gid=1000,mode=700 \
  --shm-size 256m --cap-drop ALL \
  --security-opt no-new-privileges \
  --security-opt seccomp=tests/integration/testdata/seccomp-runtime.json \
  tcdi/browser:local
docker inspect -f '{{.State.Health.Status}}' ws          # -> healthy
PORT=$(docker inspect -f '{{(index (index .NetworkSettings.Ports "8443/tcp") 0).HostPort}}' ws)
curl -sk -u "kasm_user:$(cat "$SECRETS_DIR/password")" "https://127.0.0.1:$PORT/"  # -> 200
```

## Test

```sh
go test -tags=integration ./tests/integration \
  -run 'TestLinuxRuntime' -v
```

Drives Docker via the CLI. Expects `tcdi/linux-base:it`,
`tcdi/linux-desktop:it` and `tcdi/browser:it` (tag from `:local`, or
`TCDI_IT_BUILD=1` builds missing images — the base first, the profiles
`FROM` it; `TCDI_IT_{BASE,DESKTOP,BROWSER}_IMAGE` override tags). The
image-bound suites are not part of routine CI (`ci.yml` skips them); run them
before changing anything under `build/linux-*`/`build/browser`.

- `TestLinuxRuntimeReadinessAndHome` — TLS+auth contract (on the desktop and
  the base image), display-death → not-ready, Chromium sandbox engaged (nested
  userns + seccomp filter, uid 1000), restart keeping `/home/workspace` while
  losing rootfs scratch, stale in-home credential/config not overriding the
  mounted Secret, bounded shm, no secret in logs/env.
- `TestLinuxRuntimeDesktop` — the desktop experience on a read-only root: the
  session starts, the panel and desktop windows exist (read off the window
  manager's client list with `xprop`), the panel's launchers open a terminal,
  the file manager and Firefox (the pinned build, seccomp-bpf on its content
  processes, no first-run or default-browser dialog), the desktop follows a
  RandR resize like `resize=remote`, compositing is off, nothing the contract
  forbids is installed (display manager, screensaver/lock, power manager,
  polkit agent, `sudo`, package-manager front end, any setuid/setgid file),
  and a file saved in the home survives `docker stop`/`start` and a
  recreate on the same volume.
- `TestLinuxRuntimeBrowserBaseline` — the browser image stays within 2 % of
  the pre-split package count and size
  (`tests/integration/testdata/browser-image-baseline.json`; refresh it only
  in the PR that repins the engines).
- `TestLinuxRuntimeFirefoxFallback` covers the `TCDI_BROWSER=firefox`
  fallback of the browser image: installed firefox-esr == the Dockerfile pin,
  authenticated streaming endpoint, non-root, seccomp-bpf filters engaged on
  the browser and content processes (no `MOZ_DISABLE_*SANDBOX`/`--no-sandbox`),
  no first-run/what's-new page and no extra dialog window, no in-app updater.
  The browser image ships no Firefox enterprise policy file
  (distribution/policies.json), so there is no policy to honour there (the
  desktop image does ship one; `TestLinuxRuntimeDesktop` checks its effect);
  re-run this test on every firefox-esr major bump (the freshness PR says so).

## Scan results

### 2026-10-03 (V3.26: base split + XFCE desktop) — trivy 0.70.0, syft 1.52.0, DB latest

> Later the same day `linux-base` gained three font packages
> (`fontconfig`, `fonts-noto-core`, `fonts-wqy-microhei` — font data and
> the fc tools, no new libraries; see *Budgets*). The numbers below
> predate them; the runtime-train scan job re-measures on publish.

| Image | SBOM packages | HIGH/CRITICAL with a fix (the gate) | CRITICAL / HIGH total (unfixed in bookworm) |
|---|---|---|---|
| `linux-base` | 251 | 0 | 14 / 94 |
| `linux-desktop` (XFCE4 + Firefox ESR) | 385 | 0 | 16 / 140 |
| `browser` | 375 | 0 | 17 / 153 |

The gate (`--severity CRITICAL,HIGH --ignore-unfixed`, no `.trivyignore`
entry) is clean on all three. Dispositions are as before: every remaining
finding is an OS package on bookworm with no fixed version, and the Firefox ESR
findings that exist at the pinned version are covered by the freshness check
(the desktop's pin is checked against the browser's). For comparison the old openbox/xterm `linux-desktop` scanned at 17 CRITICAL / 150
HIGH (also 0 with a fix): moving the shared layers into the base did not add
findings, and the XFCE packages bring none the gate can act on.

### 2026-09-30 (before the split) — syft 1.52.0 / grype 0.119.0


- SBOMs captured 330 / 375 packages (old linux-desktop / browser).
- Vulnerability scans (grype) from the same scan pass:
  - linux-desktop: 748 matches — Critical 42, High 168 (18 unique critical
    CVEs).
  - browser: 1163 matches — Critical 101, High 286 (43 unique critical CVEs,
    incl. 17 Chromium + 8 Firefox-ESR engine CVEs at the pinned versions).
  - **Disposition:** every Critical is an OS package on Debian bookworm with
    fix state `wont-fix`/`not-fixed` — no fixed version exists in bookworm,
    so no upgrade action is available; the exposure is inherent to the
    locked base distro. Engine CVEs are the real surface and are mitigated
    by the mandatory sandbox (measured), non-root/cap-drop runtime, and the
    gateway being the only ingress. Re-scan on each base-digest refresh.
- License scan (syft concluded/declared fields): Debian copyright is
  free-form, so ~2/3 of packages report NOASSERTION — full license audit is
  out of scan scope. Known upstream licenses of the shipped differentiators:
  KasmVNC GPL-2.0 (design §10 NOTICE/source obligations), Chromium BSD-3,
  Firefox MPL-2.0, all Debian packages DFSG-free.
- Secret-redaction: container logs + `docker inspect .Config.Env` carry zero
  secret bytes (verified by container-log and inspect review); `Config.Env` = PATH,HOME
  only.

## Image digests

Local builds are not pinned anywhere. The digests that matter are the
signed ones in `runtime-images.json` of each `runtime-*` GitHub Release (and
`images.*.digest` of a released chart).

## Known limitations

- Browser image embeds browsers via apt; Debian security updates invalidate
  the apt pins over time — repin `CHROMIUM_APT_VERSION` /
  `FIREFOX_ESR_APT_VERSION` on rebuild failures. The desktop image carries the
  same `FIREFOX_ESR_APT_VERSION`; the freshness PR repins both.
- **Desktop profile:** no desktop notifications, sound, trash or removable-media
  integration (see *Deliberately not installed*); Firefox ESR there runs with
  seccomp-bpf only (no user-namespace layer), like the browser image's
  fallback, and does not use the Chromium node-profile pair — so it is not a
  substitute for the Chromium browser template when browsing untrusted
  content; PDFs open in Firefox's built-in viewer.
- **Residual chromium findings at the .92 pin:** the trivy CRITICAL,HIGH
  gate (severities with a fix available) is clean on
  `154.0.8037.92-1~deb12u1`, but 11 HIGH engine CVEs
  (CVE-2026-102299, -102301, -102304, -102309, -102311, -102318,
  -102321, -102323, -102324, -102327, -102329) remain unfixed-in-Debian
  and are listed in the scan-job summary — the same `wont-fix`/
  `not-fixed` posture as the base-distro findings. Repin when
  bookworm-security publishes a newer build;
  `.github/scripts/check-browser-freshness.sh` fails the daily
  runtime-freshness run until the pin catches up.
- **A stale pin is also a broken build.** bookworm-security keeps only the
  newest build of each package, so once a newer one lands the pinned
  version no longer exists and `apt-get install pkg=<pin>` fails with
  `Version '<pin>' for '<pkg>' was not found` — masked while the build
  cache still holds the layer (FX-R15: firefox-esr 140.16.0esr vanished
  and the images workflow went red on the first cache miss). The freshness
  check covers **both** engines and reports that state as "pinned version
  gone".
- `network.udp` KasmVNC keys are left at upstream defaults; the streaming
  contract is websocket-only (G4a). UDP/WebRTC relay is not exercised.
- Clipboard DLP defaults to fully disabled; per-template clipboard policy
  is a later task (gateway/template layer), not image-level.
- The browser profile's Chromium sandbox needs the node security-profile
  pair (platform prerequisite C1); without it use `TCDI_BROWSER=firefox`
  (reduced isolation — seccomp-bpf only, no userns layer).
- `/run/tcdi` must be writable by uid 1000 (tmpfs uid/gid mount options or
  k8s `emptyDir` + `fsGroup`); the entrypoint fails fast otherwise.
- RFB port 5901 is not published (auth is HTTP-layer only) — same accepted
  posture.
