# tinycdi

Self-hosted virtual desktop workspaces on Kubernetes: Linux desktop/browser
sessions streamed over KasmVNC (WebSocket), behind a custom API/broker and a
Workspace operator. The Windows desktop track (KubeVirt + Guacamole/RDP) is
deferred pending its own proof gate — see `docs/compatibility.md`.

## Architecture

- **Portal** (`web/` + `build/portal`): React SPA served with the API on one
  origin; session launches open the gateway origin.
- **API** (`cmd/api`, `internal/api`): public REST surface (`/v1`),
  identity/OIDC, quotas, workspace CRUD, retained-data inventory.
- **Broker** (`internal/broker`): connection tickets (opaque, single-use,
  TTL-bound), leases, revocation, activity.
- **Gateway** (`cmd/gateway`, `internal/gateway`): session edge — ticket
  redemption, launch-origin policy, authenticated reverse proxy to the
  runtime's streaming endpoint.
- **Operator** (`cmd/operator`, `internal/operator`): reconciles `Workspace`
  / `WorkspaceTemplate` CRDs (`api/`), provisions runtime pods, enforces
  per-workspace NetworkPolicy, runs the teardown finalizer.
- **Runtime images** (`build/linux-desktop`, `build/browser`): non-root
  KasmVNC desktop on Debian bookworm, HTTPS endpoint on :8443, credentials
  via mounted secrets.

Design decisions live in `docs/architecture.md` and `docs/adr/`. Pinned
dependency/toolchain versions are in `docs/compatibility.md`.

## Layout

```
api/       Kubernetes API types (workspaces/v1alpha1) + deepcopy/CRDs source
build/     one directory per image: build/<name>/Dockerfile (context = repo root)
cmd/       Go entrypoints: api, gateway, operator
config/    kubebuilder config: CRDs, RBAC, manager, samples
deploy/    deploy/helm/tinycdi — the Helm chart (install/upgrade)
docs/      architecture, ADRs, runbooks, security policy, compatibility pins
hack/      codegen helpers (boilerplate header)
internal/  control-plane packages (api, broker, gateway, operator, runtime, ...)
tests/     integration tests (Go, envtest + Docker — no cluster required)
web/       portal SPA (React/TypeScript, Vite, vitest + Playwright mock suite)
```

## Build & test

```sh
go build ./...                                  # all Go binaries
go vet ./...                                    # vet (incl. test files)
go test -race ./internal/... ./cmd/...          # unit + envtest suites
go test -tags=integration ./tests/integration   # envtest + Docker contract tests

npm --prefix web ci && npm --prefix web test    # UI: vitest + Playwright mock suite
npm --prefix web run build                      # production bundle -> web/dist

# Every image builds from a clean checkout with repo-root context:
docker build -f build/operator/Dockerfile -t tcdi/operator .
docker build -f build/api/Dockerfile       -t tcdi/api .
docker build -f build/gateway/Dockerfile   -t tcdi/gateway .
docker build -f build/portal/Dockerfile    -t tcdi/portal .   # builds web/ in-stage
docker build -f build/linux-desktop/Dockerfile -t tcdi/linux-desktop .
docker build -f build/browser/Dockerfile   -t tcdi/browser \
    --build-arg BASE_IMAGE=tcdi/linux-desktop .
```

See `docs/images.md` for the runtime-image contract and pinned inputs.

## Deploy

Install or upgrade with the Helm chart — see `deploy/helm/tinycdi/README.md`
and the runbooks `docs/runbooks/install.md`, `docs/runbooks/upgrade.md`.
Security policy, private vulnerability reporting and mandatory hardening rules: [`SECURITY.md`](SECURITY.md).

## License

MIT — see `LICENSE`. Third-party notices: `NOTICE`,
`THIRD_PARTY_LICENSES.md`; the GPL-2.0 corresponding-source offer for the
KasmVNC binaries in the runtime images: `SOURCE-OFFER`.

TinyCDI is an independent project, not affiliated with or endorsed by
Kasm Technologies, Google or the Mozilla Foundation. "KasmVNC"/"Kasm",
"Chromium"/"Chrome" and "Firefox" are trademarks of their respective
owners, used only to identify third-party components.
