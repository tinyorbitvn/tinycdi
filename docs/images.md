# Images — build, run, test

TinyCDI ships six images, all built from `build/<name>/Dockerfile` with the
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

Scope: `build/linux-desktop/` and `build/browser/` (design §7, `docs/architecture.md`).

| Image | Contents | Purpose |
|---|---|---|
| `tcdi/linux-desktop` | `debian:bookworm-slim` (digest-pinned) + KasmVNC 1.5.0 (sha256-verified deb), openbox, xterm | general desktop profile |
| `tcdi/browser` | `FROM tcdi/linux-desktop` + `chromium` + `chromium-sandbox` + `firefox-esr` (all apt-pinned) | browser profile — session launches the browser maximized |

Pinned inputs (`docs/compatibility.md`):

- base: `debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251`
- KasmVNC: `kasmvncserver_bookworm_1.5.0_amd64.deb`, sha256 `770fd3df51510beecc89666879d82faf411276e68c6e11df612f736b891b5f71`
- Chromium: `154.0.8037.92-1~deb12u1` (apt pin — repin on Debian security updates)
- Firefox ESR (fallback): `153.4.0esr-1~deb12u1`

## Published runtime images (release train)

Runtime images ship on their own release train (`.github/workflows/
runtime-images.yml`), decoupled from control-plane `v*.*.*` releases:
every main push touching `build/linux-desktop/**` or `build/browser/**`,
weekly, and on dispatch. Signed digests are promoted to
`ghcr.io/tinyorbitvn/tinycdi-{linux-desktop,browser}:rt-YYYYMMDD.N` and
the manifest `runtime-images.json` is attached to the GitHub Release
`runtime-YYYY.MM.DD` (see `.github/README.md` for the manifest shape and
the cosign verify line). Deployments pin `images.*.digest`/`builtAt`
from that manifest — values are GitOps-owned and never written back by
the train. `runtime-freshness.yml` keeps the chromium and firefox-esr pins current
(daily check + auto PR) and fails when the newest `runtime-*` release is older
than 14 days (`docs/security/vulnerability-policy.md` §5).

## Runtime contract

- HTTPS streaming endpoint on container port **8443** (websocket transport
  only — KasmVNC 1.5.0 has no HTTP tunnel fallback). Auth = HTTP Basic against
  a kasmpasswd file holding one write-only (`w`, non-owner) user; owner-gated
  `/api/*` management routes therefore deny every request.
- `HEALTHCHECK` (`/opt/tcdi/healthcheck.sh`) succeeds only while **both** the
  X display (`xdpyinfo -display :1`) and the endpoint answer. If the display
  dies the container goes unhealthy, then exits for restart semantics.
- Credentials come from a **mounted Secret as files** under
  `/run/secrets/tcdi/` — never env, never logged:

  | file | required | use |
  |---|---|---|
  | `password` | yes | KasmVNC Basic-auth password |
  | `username` | no | default `kasm_user` |
  | `tls.crt` / `tls.key` | yes | endpoint TLS material |

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

## Build

`make docker-build-browser` builds both in order; by hand:

```sh
docker build -f build/linux-desktop/Dockerfile -t tcdi/linux-desktop:local .
docker build -f build/browser/Dockerfile -t tcdi/browser:local \
    --build-arg BASE_IMAGE=tcdi/linux-desktop:local .
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
  -run TestLinuxRuntimeReadinessAndHome -v
```

Drives Docker via the CLI. Expects `tcdi/linux-desktop:it` and
`tcdi/browser:it` (tag from `:local`, or `TCDI_IT_BUILD=1` builds missing
images; `TCDI_IT_{DESKTOP,BROWSER}_IMAGE` override tags). Covers: TLS+auth
contract, display-death → not-ready, Chromium sandbox engaged (nested
userns + seccomp filter, uid 1000), restart keeping `/home/workspace` while
losing rootfs scratch, stale in-home credential/config not overriding the
mounted Secret, bounded shm, no secret in logs/env.
`TestLinuxRuntimeFirefoxFallback` covers the `TCDI_BROWSER=firefox`
fallback: installed firefox-esr == the Dockerfile pin, authenticated
streaming endpoint, non-root, seccomp-bpf filters engaged on the browser
and content processes (no `MOZ_DISABLE_*SANDBOX`/`--no-sandbox`), no
first-run/what's-new page and no extra dialog window, no in-app updater.
The image ships no Firefox enterprise policy file (distribution/
policies.json), so there is no policy to honour; re-run this test on
every firefox-esr major bump (the freshness PR says so).

## Scan results (2026-09-30, syft 1.52.0 / grype 0.119.0, DB latest)

- SBOMs capture 330 / 375 packages (linux-desktop / browser).
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

## Image digests (local builds, not pushed)

- `tcdi/linux-desktop` image ID `sha256:0063d35ea9330f4ecb42f206b7e06219af4a2fc594133823f338a27ff11f51d1`
- `tcdi/browser` image ID `sha256:b31ca5df24fb2e8dbd22a62b9d538ad176731d669c686e981d7508c781a57542`

## Known limitations

- Browser image embeds browsers via apt; Debian security updates invalidate
  the apt pins over time — repin `CHROMIUM_APT_VERSION` /
  `FIREFOX_ESR_APT_VERSION` on rebuild failures.
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
