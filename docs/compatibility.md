# Compatibility matrix (locked versions)

Versions TinyCDI was tested with on the reference environment below.
STATIC/VENDOR-only cells were never locked.

## Environment observed (facts, not pins)

Tested with Kubernetes 1.37 (RKE2) on Ubuntu 24.04 (amd64, Linux 6.8),
containerd 2.x, Cilium 1.20, and
`kernel.apparmor_restrict_unprivileged_userns=1` on the workers.

## Locked

| Component | Locked value |
|---|---|
| Kubernetes server | v1.37.0+rke2r1 (RKE2) |
| Cilium CNI | v1.20.2 |
| Storage | ceph-csi v3.17.1, Rook v1.20.7, Ceph v20.2.4; StorageClass `rook-ceph.rbd.csi.ceph.com` (rbd, RWO, ext4, expansion on) |
| KasmVNC | 1.5.0, `kasmvncserver_bookworm_1.5.0_amd64.deb`, sha256 `770fd3df51510beecc89666879d82faf411276e68c6e11df612f736b891b5f71` (GPL-2.0; WebSocket-only transport) |
| Runtime base image | `debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251` |
| libexpat1 apt pin | `libexpat1=2.5.0-1+deb12u4` (deb.debian.org/debian-security bookworm-security) — base digest ships deb12u3 |
| Node.js / npm | v22.23.3 / npm 10.9.9 (maintenance LTS until 2027-04) |
| Playwright harness | @playwright/test 1.63.0, typescript 5.9.3, @types/node 22.20.4; bundles: Chromium 153.0.8010.12 (r1243) + headless shell, Firefox 155.0 (r1543); WebKit declared 26.6 (r2359), intentionally not installed (Safari gated) |
| nss tools | libnss3-tools 2:3.98-1ubuntu0.2 (certutil) |
| Node security profile | Localhost seccomp `deploy/node-profiles/seccomp/chromium-userns.json` sha256 `c57199218f6ba2e6616f39392f3043b3e057fdfa082ee90f0798170c5715d76a` + AppArmor `deploy/node-profiles/apparmor/tinycdi-browser` sha256 `4f30e96fc396f57ddabbe8b2ac619f47ac7074865d26bd79e0ae693049a2dd56` — required on every browser-capable worker (see conditions; install: `deploy/node-profiles/README.md`) |
| Firefox ESR fallback | firefox-esr 153.4.0esr-1~deb12u1 (deb.debian.org/debian-security bookworm-security), deb sha256 `a59e034759615e46d3b4071cd79f42642416865419319c0d2c2a35a559db5430` |

## Pending — pinned later

- **Go toolchain** — controller-runtime v0.25 + k8s.io v0.37 require
  Go >= 1.26; candidate image `golang:1.26@sha256:6c2a5538…`.
- **controller-runtime** — v0.25.x pairs with k8s.io v0.37 (server v1.37.x).
- **PostgreSQL** — postgres:18.0 digest candidate; any Postgres >= 15 works.
- **Chromium apt pin** — runtime Dockerfile installs bookworm Chromium;
  current ARG `154.0.8037.92-1~deb12u1` (latest published in
  bookworm-security as of 2026-10-01). Repin on each Debian security
  update — `.github/scripts/check-chromium-freshness.sh` fails CI while
  bookworm-security offers a newer build (see docs/images.md "Known
  limitations").

## Deferred — needs its own proof gate

- **KubeVirt** — k8s 1.37 is outside KubeVirt v1.9's upstream window; needs
  empirical validation or KubeVirt v1.10; KVM on workers unverified.
- **CDI** — additionally VolumeSnapshotClass is absent, so smart-clone is
  unproven.
- **Guacamole** — auth extension targets guacamole-ext 1.6.0 API.
- **JDK** — host OpenJDK 21.0.12.1 satisfies the guacamole-ext build
  requirement (Java >= 8) but no gate exercised it.
- **Windows image** — not provided; user input pending.

## Carry-forward conditions

1. **Node security-profile rollout** — the locked seccomp + AppArmor pair
   must be installed on every browser-capable worker before browser
   runtime pods ship; without them the Chromium sandbox silently degrades.
2. **Environment re-proof** — egress-metadata and egress-ipv6 were
   excluded in the reference environment; any environment with a link-local metadata
   endpoint or IPv6/dual-stack must re-run those gates.
3. **Windows gate stays deferred** — KubeVirt/CDI/Guacamole has its own
   proof gate; nothing here covers it.
4. **Safari/WebKit not tested** — upstream Basic-Auth-over-WebSocket
   limitation; Safari stays out of scope until upstream auth is proven.
5. **Capacity** — the measured load run is documented in
   `docs/runbooks/capacity.md`; scale beyond it needs a dedicated run
   before capacity commitments.
6. **Fixture != product** — session-scoped lease is proven; binding to
   (workspaceUID, runtimeGeneration, runtimeUID) is the locked contract.

## `hostUsers: false` — default since v0.2 (D26 / R1)

Verified 2026-10-02 on the reference environment (Kubernetes
v1.37.0+rke2r1, worker kernel 6.8.0-139-generic, containerd 2.3.4): all
three runtime cases passed with `hostUsers: false` — the desktop image
with a Retain home PVC on `ceph-block` (files survive pod delete/recreate
with `workspace:workspace` ownership via idmapped mounts), the browser
image with the Localhost seccomp + AppArmor node profiles (Chromium
renderers run with a nested user namespace and `Seccomp: 2`), and a
`kasmweb/chromium` catalog image behind the injected adapter. This is the
authoritative R1 result: `hostUsers: false` has been the shipped default
since v0.2 — the chart sets `runtime.hostUsers: false` (the operator's
`--runtime-host-users=false`), and a template's `spec.linux.hostUsers`
(D24) still overrides it per workspace.
