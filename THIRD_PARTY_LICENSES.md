# Third-party license inventory

Generated 2026-10-01. Covers every third-party component that ships in
a Linux-MVP artifact: Go module dependencies (api/operator/gateway
binaries), npm dependencies (portal bundle + dev toolchain), and the
packages inside the two runtime container images. The inventory was
produced with the pinned tools below:

| Scope | Tool (pinned) | Counted |
|---|---|---|
| Go module licenses | `go-licenses` v2.0.1 | 116 import paths |
| npm licenses — production | `license-checker` 25.0.1 | 5 packages |
| npm licenses — all incl. dev | same | 192 packages |
| Runtime image package licenses | extracted from syft SPDX SBOMs (syft 1.52.0) | 330/375 packages |
| Vulnerability scans | grype 0.119.0 | — |

Regenerate (tools on PATH):

```sh
go install github.com/google/go-licenses/v2@v2.0.1
go-licenses report ./api/... ./cmd/... ./internal/... --ignore github.com/tinyorbitvn/tinycdi
npm install -g license-checker@25.0.1
license-checker --start web --production --json
syft tcdi/linux-desktop:local -o spdx-json   # likewise tcdi/linux-base, tcdi/browser; then extract licenseDeclared per package
```

## 1. Go dependencies (all shipped binaries)

Counts over the 116 import paths reported by go-licenses:
**Apache-2.0 ×63, BSD-3-Clause ×34, MIT ×18, ISC ×1.**
No copyleft licenses in the Go dependency tree.

Direct dependencies (from `go.mod`):

| Module | License |
|---|---|
| github.com/coreos/go-oidc/v3 | Apache-2.0 |
| github.com/jackc/pgx/v5 | MIT |
| github.com/prometheus/client_golang | Apache-2.0 |
| golang.org/x/net, golang.org/x/oauth2 | BSD-3-Clause |
| gopkg.in/yaml.v3 (+ go.yaml.in/yaml/v3) | MIT / Apache-2.0 |
| k8s.io/{api,apiextensions-apiserver,apimachinery,client-go} | Apache-2.0 |
| sigs.k8s.io/controller-runtime | Apache-2.0 |

## 2. npm dependencies (portal)

**Production bundle** (what ships in
`web/dist`): react 19.3.0, react-dom 19.3.0, scheduler, openapi-fetch
0.17.0, openapi-typescript-helpers — **all MIT**. No copyleft in the
portal bundle.

**All dependencies incl. dev** (192 packages):
MIT ×163, Apache-2.0 ×9, ISC ×9, MIT-0 ×2, BSD-2-Clause ×2, BSD-3-Clause
×2, plus one each of Python-2.0, CC-BY-4.0, BlueOak-1.0.0, CC0-1.0,
(MIT OR CC0-1.0). `tinycdi-portal` itself is **MIT** — the project's
own license (see LICENSE; declared in `web/package.json`). The
production bundle ships `web/dist/THIRD_PARTY_LICENSES.txt`, generated
at build time.

### 2a. Vendored web assets (web/src/design/orbit/)

| Asset | Source (pinned) | License |
|---|---|---|
| Orbit stylesheets `css/*.css` | TinyOrbit Orbit System 2.9 (exported 2026-10-02) | TinyOrbit design-system asset; ships with this project |
| Manrope Variable WOFF2 subsets | `@fontsource-variable/manrope` 5.3.0 | OFL-1.1 (`LICENSES/manrope-LICENSE.txt`) |
| JetBrains Mono 400 WOFF2 subsets | `@fontsource/jetbrains-mono` 5.3.0 | OFL-1.1 (`LICENSES/jetbrains-LICENSE.txt`) |
| Tabler Icons outline SVGs | `@tabler/icons` 3.48.0 | MIT (`LICENSES/LICENSE-Tabler.txt`) |

SHA-256 of every vendored file is recorded in
`web/src/design/orbit/README.md` and pinned by
`web/tests/unit/design/tokens.test.ts`. The TinyOrbit name, wordmark and
mark are trademarks and are not MIT-licensed — see `TRADEMARKS.md`.

## 3. Runtime container images

Per-package declared licenses were extracted from syft SPDX SBOMs of
the runtime images: the shared linux-base image carries 251 packages, the
browser image 375 and the linux-desktop image (XFCE4 + Firefox ESR) 385 —
both profiles are built FROM linux-base.

These are full Debian bookworm userspaces, so they contain the normal
mix of copyleft and permissive system components — declared-license
counts of copyleft-bearing packages (measured before the base/profile
split; the shared base is a subset of both): ~220 (browser) / ~190
(old openbox-based linux-desktop) packages declare GPL/LGPL-family terms, overwhelmingly
GPL-2.0/GPL-3.0/LGPL-2.1 system libraries and tools (glibc, coreutils,
bash, openssl-adjacent libs, etc.). Key packages:

| Package | Version | Declared license |
|---|---|---|
| kasmvncserver | 1.5.0-1 | **GPL-2.0-only AND GPL-2.0-or-later** |
| libexpat1 (apt-pinned in linux-base) | 2.5.0-1+deb12u4 | MIT |
| @kasmtech/novnc | 1.3.0 | MPL-2.0 |
| chromium, chromium-common, chromium-sandbox (browser image) | 154.0.8037.92-1~deb12u1 | mixed BSD/MIT/Apache/GPL/LGPL/MPL — see `/usr/share/doc/<pkg>/copyright` in the image |
| firefox-esr (browser and linux-desktop images) | 153.4.0esr-1~deb12u1 | MPL-2.0-led mixed — see `/usr/share/doc/<pkg>/copyright` |
| openbox (browser image) | 3.6.1-10 | BSD-3-Clause AND GPL-2.0/GPL-3.0 |
| xfce4-session, xfwm4, xfce4-panel, xfdesktop4, xfce4-settings, thunar, xfce4-terminal (linux-desktop image) | 4.18.x / 1.0.4 | GPL-2.0-or-later, LGPL-2.1+ (per package copyright file) |
| mousepad, xarchiver, ristretto (linux-desktop image) | 0.5.10 / 0.5.4 / 0.12.4 | GPL-2.0-or-later, LGPL-2.0+/LGPL-3+ |
| greybird-gtk-theme (linux-desktop image) | 3.23.2-1 | GPL-2.0-or-later or CC-BY-SA-3.0-or-later |
| adwaita-icon-theme (linux-desktop image) | 43-1 | CC-BY-SA-3.0 or LGPL-3.0 |

Control-plane images (api, operator, gateway, portal) are
`gcr.io/distroless/static:nonroot` + a static Go binary — Go deps only,
no copyleft expected. Each also carries `/licenses/` — the collected
license texts of its Go dependency tree (`make licenses`, go-licenses
v2.0.1). The CI `images` job scans every image in the set on every build.

### 3a. Derived node security profiles (deploy/node-profiles/)

| Artifact | Derived from | License |
|---|---|---|
| `seccomp/chromium-userns.src.json` (+ resolved `chromium-userns.json`) | moby `profiles/seccomp/default.json` @ v27.5.1 | Apache-2.0 |
| `apparmor/tinycdi-browser` | containerd `contrib/apparmor/template.go` @ 18c051a7 (v2.3.4) | Apache-2.0 |

Both ship as node-installed Localhost profiles (not image-bundled); each
file header names the pinned upstream source.

## 4. Copyleft obligations — read before distributing

1. **KasmVNC (GPL-2.0).** Both runtime images install the upstream
   `kasmvncserver_bookworm_1.5.0_amd64.deb` (sha256 pinned in
   `docs/compatibility.md`, KasmVNC entry). Distributing
   the image distributes GPL-2.0 binaries → we provide **corresponding
   source**: `hack/kasmvnc-source-bundle.sh` assembles
   `kasmvnc-1.5.0-corresponding-source.tar.gz` (the v1.5.0 source,
   kasmweb/noVNC submodule, and the build-time-fetched deps — all
   sha256/commit-pinned) for publication with each release, and
   NOTICE/`SOURCE-OFFER` carry a written offer valid for at least three
   years. The notices are also inside the images under
   `/usr/share/doc/tinycdi/`. We distribute the .deb **unmodified** —
   if a build ever patches KasmVNC, the patched source must be published
   under GPL-2.0 with the image.
2. **Debian base packages (GPL/LGPL).** Debian makes corresponding
   source for every shipped binary package available via its source
   archives; our NOTICE references `deb-src` for the `bookworm` suite as
   the corresponding-source mechanism for the distro packages we
   redistribute unmodified inside the images.
3. **MPL-2.0 (noVNC, Firefox ESR).** Weak copyleft: ship license text,
   keep MPL-covered files' source available (upstream publishes it);
   our usage is unmodified distribution — no additional obligations
   triggered, but the license notices must accompany the distribution.
4. **Chromium.** BSD-led with bundled GPL/LGPL/MPL components —
   distributable; attribution carried in the image SBOM.
5. **Portal bundle + Go binaries.** Permissive only — attribution
   (license texts retained) is the sole obligation.
6. **Project license — MIT** (LICENSE). The MIT
   project code does not change the obligations for the GPL-2.0 KasmVNC
   binaries redistributed in the runtime images (source offer above).
