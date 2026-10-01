# Kasm workspace images (`spec.linux.adapter: kasm`)

TinyCDI can run **unmodified** `kasmweb/*` workspace images (the public
kasmtech/workspaces-images catalog on Docker Hub) as `LinuxContainer`
runtime images. The mechanism — "approach A" — keeps the entire TinyCDI
runtime contract and ADR 0001 security model: the image's own Kasm
startup (`/dockerstartup/vnc_startup.sh`) is bypassed completely, because
it would create an owner-capable VNC account, bake a self-signed cert,
listen on :6901, and start side services (audio, upload, gamepad, webcam,
printer, smartcard) that must not exist in a locked-down runtime.

The adapter is a ~200-line shim delivered **into** the pod, not into the
image:

```
initContainers:
- name: tcdi-adapter-init
  image: <kasmAdapter.image — digest-pinned, operator flag>
  command: ["/install-adapter"]     # writes the adapter scripts into the shared volume
  volumeMounts: [{name: tcdi-adapter, mountPath: /opt/tcdi}]
containers:
- name: desktop
  image: kasmweb/<app>@sha256:…     # spec.linux.image — digest pin enforced by the CRD
  command: ["/opt/tcdi/entrypoint.sh"]
  env:
  - {name: HOME, value: /home/workspace}
  - {name: TCDI_SESSION_CMD, value: "<spec.linux.sessionCmd>"}   # only when set
  - {name: VNC_PW, value: ""}       # neutralize the image's baked-in defaults
  - {name: VNC_VIEW_ONLY_PW, value: ""}
  volumeMounts: + {name: tcdi-adapter, mountPath: /opt/tcdi, readOnly: true}
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
writes three scripts into the shared volume:

- `entrypoint.sh` — same contract as `build/linux-desktop/entrypoint.sh`:
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
- `healthcheck.sh` — readiness gate: `xdpyinfo` on :1 AND an HTTPS answer
  on :8443 (tolerant of KasmVNC 1.4.x's loopback blacklist quirk).
- `xstartup.sh` — starts the WM (`startxfce4`, present in all kasm
  desktop/app images) plus `$TCDI_SESSION_CMD` when set.

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
  binaries — under the adapter the only listening socket is :8443, but
  the binaries remain in the filesystem.
- `VNC_PW`/`VNC_VIEW_ONLY_PW` are baked into the image env; the pod
  neutralizes them to empty.

## Onboarding checklist for a new kasm image

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
3. **trivy gate.** Scan the digest-pinned image with the release-gate
   flags (`--severity CRITICAL,HIGH --ignore-unfixed`). Images that fail
   the gate are not catalog entries — e.g. `kasmweb/ubuntu-noble-desktop`
   carries hundreds of fixable HIGH/CRITICAL findings today.
4. **Contract test.** Run the adapter contract suite against the image:
   `TCDI_IT_KASM_IMAGE=<repo>@sha256:<digest> go test -tags=integration
   ./tests/integration -run TestKasmAdapterChromium` — it proves the
   auth/TLS/401 surface, the write-only user (owner-gated `/api/*` → 401),
   uid 1000 + sandboxed renderers, and home persistence. For
   browser-family images the renderer probe additionally proves the
   wrapper bypass (no `--no-sandbox` process may exist).
5. **Digest tracking.** Catalog entries pin `kasmweb/<app>@sha256:…`.
   Track the upstream `X.Y.Z-rolling-weekly` tags (frozen `X.Y.Z` releases
   lag security refreshes; `-rolling-daily` is too noisy): resolve the
   tag to a new digest on a schedule, re-run the trivy gate + contract
   test in CI, then bump `spec.linux.image` via a PR. On a KasmVNC
   version bump, re-check the healthcheck loopback quirk and the yaml
   schema keys the adapter writes.
6. **Node profiles.** Browser-family images additionally need the
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
- Chart: `kasmAdapter.image.{repository,tag,digest}` → the flag. The
  digest is required; see `deploy/helm/tinycdi/README.md` ("Kasm
  workspace images") and `ci/example-values.yaml` for a complete seeded
  `kasmweb/chromium` template.
- CRD fields: `spec.linux.adapter` (`""`|`"kasm"`, default `""`),
  `spec.linux.sessionCmd` (only with `adapter=kasm`; `spec.linux.command`
  is forbidden with `adapter=kasm` — the adapter supplies the command).

## Known limitations

- The pod-level composition is what CI proves via the docker-based
  contract test; a kind-based end-to-end run of the full rendered pod
  spec is a follow-up.
- Not every catalog entry has been exercised — the per-image checklist
  above is the gate.
