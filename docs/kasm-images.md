# Kasm workspace images (`spec.linux.adapter: kasm`)

TinyCDI can run **unmodified** `kasmweb/*` workspace images (the public
kasmtech/workspaces-images catalog on Docker Hub) as `LinuxContainer`
runtime images. The mechanism — "approach A" — keeps the entire TinyCDI
runtime contract and ADR 0001 security model: the image's own Kasm
startup (`/dockerstartup/vnc_startup.sh`) is bypassed completely, because
it would create an owner-capable VNC account, bake a self-signed cert,
listen on :6901, and start side services (audio, upload, gamepad, webcam,
printer, smartcard) that must not exist in a locked-down runtime.

> **Status: preview.** `kasmweb/*` browser images are a preview feature,
> not a parity alternative to the native images: the bundled engine lags
> upstream by up to 4 major versions (the tolerated budget in the
> freshness floor below), so a kasm browser is inherently behind on
> web-engine security fixes. For browsing untrusted sites use the native
> `tinycdi-browser` image, which tracks the Debian chromium-security pin.
> Kasm images exist for the apps/desktops that have no native equivalent.

The adapter is a ~200-line shim delivered **into** the pod, not into the
image:

```
initContainers:
- name: tcdi-adapter-init
  image: <kasmAdapter.image — digest-pinned, operator flag>
  command: ["/install-adapter"]     # writes the adapter scripts into the shared volume
  resources: {requests: {cpu: 10m, memory: 32Mi, ephemeral-storage: 16Mi},
              limits:   {cpu: 10m, memory: 32Mi, ephemeral-storage: 16Mi}}
  securityContext: {readOnlyRootFilesystem: true, …hardened baseline…}
  volumeMounts: [{name: tcdi-adapter, mountPath: /opt/tcdi}]
containers:
- name: desktop
  image: kasmweb/<app>@sha256:…     # spec.linux.image — digest pin enforced by the CRD
  command: ["/opt/tcdi/entrypoint.sh"]
  securityContext: {readOnlyRootFilesystem: true, …same baseline…}
  env:
  - {name: HOME, value: /home/workspace}
  - {name: TCDI_SESSION_CMD, value: "<spec.linux.sessionCmd>"}   # only when set
  - {name: VNC_PW, value: ""}       # neutralize the image's baked-in defaults
  - {name: VNC_VIEW_ONLY_PW, value: ""}
  volumeMounts:
  - {name: tcdi-adapter, mountPath: /opt/tcdi, readOnly: true}
  # sandbox-preserving shim over every Chromium-family wrapper path
  # (Kasm's /usr/bin/chromium etc. hardcode --no-sandbox):
  - {name: tcdi-adapter, mountPath: /usr/bin/chromium, subPath: browser-shim.sh, readOnly: true}
  - {name: tcdi-adapter, mountPath: /usr/bin/chromium-browser, subPath: browser-shim.sh, readOnly: true}
  - {name: tcdi-adapter, mountPath: /usr/bin/google-chrome, subPath: browser-shim.sh, readOnly: true}
  - # … full list: AdapterBrowserWrappers in internal/runtime/linux/backend.go
  # managed Chromium policy (warnings on, devtools/extension-installs off):
  - {name: tcdi-adapter, mountPath: /etc/chromium/policies/managed/zz-tcdi.json, subPath: chromium-policy.json, readOnly: true}
  - # … full list: AdapterPolicyTargets in internal/runtime/linux/backend.go
volumes: + {name: tcdi-adapter, emptyDir: {}}
```

Everything else in the pod is identical to a native runtime: port 8443,
the exec readiness probe (`/opt/tcdi/healthcheck.sh`), the Secret mount
at `/run/secrets/tcdi`, the tmpfs `/run/tcdi`, bounded `/dev/shm`, home
volume at `/home/workspace`, `securityContext` (uid 1000, drop ALL,
no-new-privs, seccomp/AppArmor), Service and NetworkPolicy.

## Adapter contract

The adapter image (`build/kasm-adapter/`, published as
`tinycdi-kasm-adapter`) contains a static `install-adapter` copier that
writes the adapter files into the shared volume:

- `entrypoint.sh` — same contract as `build/linux-base/entrypoint.sh`:
  reads credentials/TLS from the mounted Secret files, stages them on the
  ephemeral `/run/tcdi` mount, builds a **write-only non-owner**
  `kasmvncpasswd` file (owner rights would unlock KasmVNC's `/api/*`
  management surface), seeds `/home/kasm-default-profile` into the
  persistent home on first boot, neutralizes `~/.kasmpasswd`
  (symlink → `/run/tcdi`), rewrites `~/.vnc/kasmvnc.yaml` every boot with
  the full TinyCDI policy (port 8443, TLS pem → `/run/tcdi`, clipboard
  DLP off, client overrides off, COEP/COOP headers, brute-force 5/10),
  then runs `kasmvncserver :1 -xstartup /opt/tcdi/xstartup.sh
  -select-de manual -SecurityTypes None -websocketPort 8443 -interface
  0.0.0.0 -sslOnly` and supervises Xvnc.
- `healthcheck.sh` — readiness gate: `xdpyinfo` on :1 AND an authenticated
  HTTPS 200 on :8443, logging in with the mounted Secret (an anonymous probe
  is an authentication failure to KasmVNC and blacklists loopback; tolerant of
  KasmVNC 1.4.x's loopback quirk where curl prints the code and exits non-zero).
- `xstartup.sh` — with `sessionCmd` set, starts ONLY a window manager
  (`xfwm4`, falling back to `openbox`/`startxfce4`) plus the payload —
  no desktop icons, panel launchers or app menu survive as unsandboxed
  relaunch surfaces — and when the payload exits the display is torn
  down (the container exits and restarts into a fresh session, same
  lifecycle as the native browser image). Without `sessionCmd`, the
  image's XFCE session runs.
- `browser-shim.sh` — bind-mounted read-only (subPath) over every
  Chromium-family wrapper path (`/usr/bin/chromium`,
  `/usr/bin/google-chrome`, `/usr/bin/microsoft-edge`, … —
  `AdapterBrowserWrappers`). Every relaunch path — desktop icon, menu,
  `gtk-launch`, `xdg-open`, `x-www-browser`/`sensible-browser` — execs
  the REAL binary with `--no-sandbox`/`--disable-*-sandbox`/
  `--single-process` stripped. Unreachable to unmount for the session
  user (ro mount, no CAP_SYS_ADMIN). It resolves the real binary from
  `/run/tcdi/browser-bin` (the sessionCmd payload binary, written by
  entrypoint.sh) or a built-in candidate list.
- `chromium-policy.json` — managed policy mounted into each engine's
  policy dir (`AdapterPolicyTargets`, filename `zz-tcdi.json` so it
  sorts last): re-enables the `--no-sandbox` warning bar, Safe Browsing
  standard, disables DevTools, extension installs, incognito, sync.

Notable deltas vs the tcdi/* images (all proven on
`kasmweb/chromium:1.18.0` and `kasmweb/ubuntu-noble-desktop:1.18.0`):

- **KasmVNC version follows the image**, not our pin — 1.18.0 images
  carry KasmVNC 1.4.0, which needs `-select-de manual` to honor
  `-xstartup` and lacks
  `server.allow_environment_variables_to_override_config_settings` (the
  adapter emits that key only when the image's schema has it). The
  policy root moves from immutable `/etc/kasmvnc/kasmvnc.yaml` to
  `$HOME/.vnc/kasmvnc.yaml` rewritten each boot — it merges after /etc
  and wins for every key we set.
- Kasm images ship dormant upload/audio/gamepad/webcam/printer/smartcard
  binaries — under the adapter the only listening sockets are :8443/tcp
  and Xvnc's UDP transport on :8443/udp (KasmVNC 1.4.x cannot disable it —
  `network.udp.port` accepts only `auto`|int and 0 resolves to the
  websocket port; the TCP-only NetworkPolicy is the control, same
  residual as the native images), but the binaries remain in the
  filesystem.
- **Rootfs is read-only** (`readOnlyRootFilesystem: true` on the desktop
  and init containers): the kasmweb images ship world-writable policy
  files and a uid-1000-writable dir inside the served web root
  (`/usr/share/kasmvnc/www/Downloads`); the ro rootfs kills both, plus
  any in-image tampering. All session writes land on mounts (home,
  `/run/tcdi`, `/tmp`, `/dev/shm`).
- `VNC_PW`/`VNC_VIEW_ONLY_PW` are baked into the image env; the pod
  neutralizes them to empty.

## Onboarding checklist for a new kasm image

The catalog is `build/kasm-catalog.txt` — every `kasmweb/*` image
referenced anywhere in tracked files (chart ci values, docs, examples)
must appear there, digest-pinned, with an engine + freshness floor.
`.github/scripts/check-kasm-catalog.sh` enforces that in every PR; the
heavyweight `.github/scripts/scan-kasm-catalog.sh` (trivy gate + engine
extraction) and the adapter contract test run in the `kasm-contract`
ci.yml job — weekly, on dispatch, on main pushes, and on PRs touching
the adapter/catalog surface.

Every `kasmweb/<app>` added to the catalog must clear this list **before
its template merges**:

1. **Wrapper audit.** Check whether the app's launch binary is a Kasm
   wrapper that hardcodes `--no-sandbox` or other unsafe flags —
   Chromium-family images (chromium, chrome, edge, brave, vivaldi) all
   ship one. `sessionCmd` must then point at the REAL binary
   (`/usr/bin/chromium-orig`, `/opt/google/chrome/google-chrome`, …), not
   the wrapper. Env flags (`CHROME_FLAGS`, `APP_ARGS`) cannot remove
   baked-in flags — only the binary path bypass works.
2. **`sessionCmd`.** Choose and pin the session payload command
   (`spec.linux.sessionCmd` → `TCDI_SESSION_CMD`). For full-desktop
   images it may stay empty (XFCE session). Printable ASCII, ≤512 chars
   (CRD-enforced).
3. **trivy gate.** `scan-kasm-catalog.sh` scans the digest-pinned image
   with the release-gate flags (`--severity CRITICAL,HIGH
   --ignore-unfixed`) plus the catalog-scoped
   `build/kasm-catalog.trivyignore` (same SEC-44 expiry rules; dormant-
   path exceptions only). Images that fail are not catalog entries —
   e.g. `kasmweb/ubuntu-noble-desktop` carries hundreds of fixable
   HIGH/CRITICAL findings today and is listed **unsupported** in the
   catalog. NOTE: trivy's OS-package view does NOT cover the bundled
   browser engine reliably (a Debian chromium inside an Ubuntu base
   reports 0 vulns) — the freshness floor below is the engine gate.
4. **Engine freshness floor.** The catalog entry's `min-engine-major`
   must be >= the major of `build/browser`'s `CHROMIUM_APT_VERSION`
   minus 4 (~4 upstream release cycles of tolerated lag on Debian/Ubuntu
   rebuilds), and the installed engine must meet it. This is a blocking
   CI gate and stays blocking — the ≤4-major lag is the accepted
   residual risk of the preview, not a target to grow. The seeded
   `kasmweb/chromium` carries Chromium 150 vs the native pin's 154 —
   inside the budget; a stale digest (e.g. the Oct-2025 pin, Chromium
   139) is rejected.
5. **Contract test.** Run the adapter contract suite against the image:
   `TCDI_IT_KASM_IMAGE=<repo>@sha256:<digest> go test -tags=integration
   ./tests/integration -run TestKasmAdapterChromium` — it proves the
   auth/TLS/401 surface, the write-only user (owner-gated `/api/*` → 401),
   uid 1000, EVERY renderer sandboxed (nested userns + stacked seccomp
   filters — assertions fail on a single unsandboxed renderer), relaunch
   through the wrapper paths staying sandboxed, session teardown on
   payload exit, ro rootfs, and home persistence.
6. **Digest tracking.** Catalog entries pin `kasmweb/<app>@sha256:…`.
   Track the upstream `X.Y.Z-rolling-weekly` tags (frozen `X.Y.Z` releases
   lag security refreshes; `-rolling-daily` is too noisy): resolve the
   tag to a new digest on a schedule — the weekly `kasm-contract` job
   fails when the pinned digest stops passing the scan or the contract —
   then bump `spec.linux.image` via a PR. Cataloged digests follow the
   same 14-day image-age rule as the runtime train: a pin older than
   14 days is refreshed to the current rolling-weekly digest even when
   every gate still passes. On a KasmVNC version bump,
   re-check the healthcheck loopback quirk and the yaml schema keys the
   adapter writes.
7. **Node profiles.** Browser-family images additionally need the
   Localhost seccomp + AppArmor pair on the nodes
   (`nodeProfiles.install` / `deploy/node-profiles/`) for the in-session
   sandbox — same prerequisite as the tcdi browser image.

## Licensing

- **Never republish `kasmweb/*` images.** The workspaces-images Docker
  recipes are MIT, but the `kasmweb/core-*` base layers contain prebuilt
  Kasm service binaries pulled from `kasmweb-build-artifacts.s3` whose
  license is **not stated** — building a derived image ("approach B") and
  pushing it is uncleared. Approach A avoids redistribution entirely:
  cluster nodes pull `kasmweb/*` straight from Docker Hub (or a
  pull-through cache — get counsel sign-off on caching).
- **App payloads.** Proprietary app images (chrome, edge, vs_code, zoom,
  teams, discord, and every desktop image bundling google-chrome/MS
  code/sublime — verify per image) may be *run* via approach A but never
  rebuilt/republished.
- The adapter scripts are original TinyCDI code (MIT). If Kasm script
  text is ever copied into `build/kasm-adapter/`, carry the MIT notice
  and add an entry to `NOTICE`/`THIRD_PARTY_LICENSES.md`.
- `tinycdi-kasm-adapter` itself is `distroless/static` + a static Go
  copier + the three adapter scripts — MIT.

## Operator / chart wiring

- Operator flag `--kasm-adapter-image` (env `TCDI_KASM_ADAPTER_IMAGE`):
  digest-pinned reference of the adapter image; validated at startup.
  Unset → `adapter: kasm` templates are rejected (`ErrTemplateRejected`).
- Chart: `kasmAdapter.enabled` (default `false`) gates the flag, and
  `kasmAdapter.image.{repository,tag,digest}` supplies its value. The
  adapter is **off by default**: the released chart carries a stamped
  `kasmAdapter.image.digest`, but the operator only gets
  `--kasm-adapter-image` when `enabled` is `true`. Enabling requires the
  digest (the render fails otherwise), and a seeded `adapter: kasm`
  template requires `enabled: true`. See `deploy/helm/tinycdi/README.md`
  ("Kasm workspace images") and `ci/example-values.yaml` for a complete
  seeded `kasmweb/chromium` template.

  To enable it:

  ```yaml
  kasmAdapter:
    enabled: true            # image.digest is already stamped in the released chart;
                             # set image.repository/image.digest for a self-built or mirrored image
  ```
- CRD fields: `spec.linux.adapter` (`""`|`"kasm"`, default `""`),
  `spec.linux.sessionCmd` (only with `adapter=kasm`; `spec.linux.command`
  is forbidden with `adapter=kasm` — the adapter supplies the command).

## Known limitations

- The pod-level composition is what CI proves via the docker-based
  contract test; a kind-based end-to-end run of the full rendered pod
  spec is a follow-up.
- Not every catalog entry has been exercised — the per-image checklist
  above is the gate.
