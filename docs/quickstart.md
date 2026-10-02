# Quickstart on kind

A complete, working TinyCDI on your own machine in well under 30 minutes:
one script creates a [kind](https://kind.sigs.k8s.io/) cluster, an ingress
controller, PostgreSQL and a dev Keycloak, generates a local CA and wildcard
certificate, builds the platform images from your checkout and installs the
chart from it.

> **Dev only.** The quickstart is for trying TinyCDI and for development. It
> runs a throwaway cluster, a Keycloak in `start-dev` mode with a published
> test login, a self-made CA, workspaces on the same node as everything else
> (`runtime.placement.allowSharedNodes: true`), Firefox instead of the
> sandboxed Chromium, and no AppArmor or user-namespace confinement of the
> workspace pods. Never point it at a shared cluster and never
> reuse anything from it. For a real install follow
> [runbooks/install.md](runbooks/install.md).

## What you need

| Tool | Version | Notes |
|---|---|---|
| Docker | recent (Docker Desktop works) | ~10 GB free disk, 4 CPUs and 8 GB RAM for Docker |
| kind | v0.33.0 or newer | the script pins the Kubernetes node image by digest |
| kubectl | any recent | |
| Helm | v4 | the chart requires Helm 4 |
| openssl, curl | any recent | |

Ports **80** and **443** on `127.0.0.1` must be free — kind publishes the
ingress controller there. A browser (and `curl`) must be able to resolve
`*.tcdi.localtest.me`; that wildcard name resolves to `127.0.0.1` on the public
DNS, including the per-workspace session hosts
`ws-<hex>.session.tcdi.localtest.me`. Offline, add the names you use to
`/etc/hosts`, or point the domain somewhere else with `TCDI_QS_DOMAIN`.

## Run it

```sh
git clone https://github.com/tinyorbitvn/tinycdi && cd tinycdi
hack/quickstart/up.sh
```

On a typical machine the first run takes 8–15 minutes (image builds and pulls
dominate; they run in parallel). It ends with:

```text
  Portal   https://portal.tcdi.localtest.me
  Login    demo / tcdi-demo-dev-only   (dev realm; exists only inside kind cluster tcdi-quickstart)
  CA       ~/.local/state/tcdi-quickstart/tcdi-quickstart/tls/local-ca.crt
```

Open the portal, accept the certificate warning (or import the CA file),
log in, choose **New workspace**, pick **Browser (quickstart)** and press
**Create workspace**. When the workspace is ready, **Connect** opens the
desktop in the portal.

The login `demo` / `tcdi-demo-dev-only` is published here on purpose: it
exists only in the Keycloak realm of this throwaway cluster, which listens on
loopback only. The realm (`hack/quickstart/realm-export.json`) maps the
user's `tenant_id` attribute to the `tenant-a` namespace.

## Clean up

```sh
hack/quickstart/down.sh
```

Deletes the kind cluster (and every workspace, volume and Secret in it), the
images `up.sh` built, and the state directory
(`~/.local/state/tcdi-quickstart/<cluster>`: private kubeconfig, local CA and
generated passwords). It is safe to run twice. Docker's build cache is left
alone (`docker builder prune` reclaims it).

## What `up.sh` creates

| Piece | How |
|---|---|
| kind cluster `tcdi-quickstart` | one node, Kubernetes v1.37 image pinned by digest, host ports 80/443 on loopback → ingress |
| Ingress | Traefik (Helm chart `41.6.1`, checksum-verified, image pinned by digest) — it terminates browser TLS and re-encrypts to the TinyCDI pods |
| PostgreSQL | `postgres:18.0` by digest with TLS from the quickstart CA; the backend connects with `verify-full` |
| Identity | Keycloak 26.8.0 in `start-dev` mode, realm `tinycdi` imported from `realm-export.json`: one confidential client and the `demo` user |
| TLS | two local CAs (edge/Postgres, internal mTLS) and a leaf certificate covering `portal.`, `keycloak.` and `*.session.` names under the domain |
| DNS inside the cluster | a CoreDNS rewrite sends `*.tcdi.localtest.me` to the ingress Service, so pods reach the OIDC issuer under the same name the browser uses |
| TinyCDI | `helm upgrade --install` of `deploy/helm/tinycdi` from your checkout with `hack/quickstart/values.yaml`, plus two seeded templates (browser, Linux desktop) using the published runtime images pinned by digest |

Images: `up.sh` **builds** the backend, operator and frontend from your working
tree and `kind load`s them, so the chart and the images always match, even on a
branch that is ahead of the last release. The browser and desktop runtime
images are the published runtime release train, pinned by digest in
`hack/quickstart/common.sh`, and are pre-pulled into the node while the build
runs. To skip the build on a release checkout use the published images:

```sh
TCDI_QS_IMAGES=published TCDI_QS_IMAGE_TAG=<release> hack/quickstart/up.sh
```

Everything the script downloads is pinned by version, and by digest or
checksum where the registry offers one (`common.sh` holds the pins).

## Options

| Variable | Default | Effect |
|---|---|---|
| `TCDI_QS_CLUSTER` | `tcdi-quickstart` | kind cluster name |
| `TCDI_QS_DOMAIN` | `tcdi.localtest.me` | base domain: `portal.`, `keycloak.`, `session.` are derived |
| `TCDI_QS_IMAGES` | `build` | `build` or `published` |
| `TCDI_QS_IMAGE_TAG` | chart appVersion | tag for `published` images |
| `TCDI_QS_STATE_DIR` | `~/.local/state/tcdi-quickstart` | kubeconfig, CA, passwords, logs |

Re-running `up.sh` on an existing cluster rebuilds the images and upgrades the
release in place — the fast way to try a change.

## Poking around

```sh
export KUBECONFIG=~/.local/state/tcdi-quickstart/tcdi-quickstart/kubeconfig
kubectl -n tinycdi-system get pods
kubectl -n tinycdi-tenant-a get workspaces,pods
kubectl get workspacetemplates -A
```

The Keycloak admin console is at `https://keycloak.tcdi.localtest.me` (user
`admin`; the password is in the `keycloak-admin` Secret of namespace
`tcdi-qs-deps`).

## How kind differs from a real cluster

The quickstart values (`hack/quickstart/values.yaml`) bend the chart in a few
places because a kind node is a container, not a machine. None of these belong
in a production values file:

| Setting | Why on kind |
|---|---|
| `runtime.placement.allowSharedNodes: true` | there is no dedicated workspace node pool |
| `runtime.appArmor.requireRuntimeDefault: false` | kind nodes have no AppArmor, so the pods cannot carry the `RuntimeDefault` AppArmor profile (kubelet refuses them) |
| `runtime.hostUsers: null` | kind nodes cannot start user-namespaced pods (runc fails mounting `sysfs`); production keeps `hostUsers: false` |
| browser template runs Firefox ESR (`command: [env, TCDI_BROWSER=firefox, …]`) | Chromium's sandbox needs the Localhost seccomp/AppArmor node profiles, which a kind node cannot load; Firefox runs with seccomp-bpf only — reduced isolation, see [images.md](images.md) |
| `networkPolicy.apiServerPeers` = the control-plane endpoint IP, `apiServerPort` = its port (6443) | kindnet enforces NetworkPolicy *after* the `kubernetes` Service is NAT-ed, so the chart's default (Service IP `10.96.0.1`, port 443) never matches; `up.sh` reads the endpoint from the cluster |
| `oidc.port: 8443` | same reason: the egress rule towards the ingress controller has to name Traefik's container port, not Service port 443 |
| `tenant_quota` row inserted by SQL | the API has no quota endpoint yet and a tenant without a quota row is refused every create; `up.sh` seeds `tenant-a` (3 workspaces, 4 CPU, 8 GiB, 50 GiB) the way [runbooks/capacity.md](runbooks/capacity.md) describes |
| Traefik `--serversTransport.insecureSkipVerify` and `traefik.ingress.kubernetes.io/service.serversscheme: https` | the TinyCDI pods terminate TLS themselves with a quickstart-CA certificate; Traefik re-encrypts to them without verifying names |

Because of the NetworkPolicy findings above, a CNI that enforces policy
before Service NAT (Cilium, Calico) needs the Service IP and port 443 instead —
exactly what the chart defaults to.

## Troubleshooting

- **Port 80/443 already in use** — stop the other listener or use another
  machine; `up.sh` refuses to start rather than half-install.
- **The browser says "server not found"** — your resolver filters
  `localtest.me`; add `127.0.0.1 portal.tcdi.localtest.me keycloak.tcdi.localtest.me`
  and one line per workspace host to `/etc/hosts`, or use a domain you control.
- **A step failed** — `up.sh` prints the failing step and keeps its logs in
  `<state dir>/logs`. `down.sh` always cleans up, whatever state the run ended in.
- **Workspace stays in Starting** — the first desktop boot pulls the runtime
  image if the background pre-pull did not finish: watch
  `kubectl -n tinycdi-tenant-a get pods -w`.

## CI

The `quickstart` job in `.github/workflows/ci.yml` runs `up.sh` under a
20-minute budget on a stock runner, drives the portal with the Playwright
smoke in `hack/quickstart/smoke/` (log in, create a browser workspace, wait for
the session frame) and then runs `down.sh` twice, failing if any kind cluster
remains. It runs on pushes to `main`, on manual dispatch and on pull requests
that touch `hack/quickstart/`, `deploy/helm/` or `build/`; it is not a required
check.
