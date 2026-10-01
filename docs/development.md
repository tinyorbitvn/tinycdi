# Development

Building, testing and iterating on TinyCDI. For the platform design see
`docs/architecture.md`; for pinned toolchains and inputs see
`docs/compatibility.md`.

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
```

The integration suite runs the real runtime images under Docker — envtest
plus containers, no cluster required. See
`tests/integration/linux_runtime_test.go` for the exact container contract
(mounted TLS material + credentials, seccomp profile, dropped caps).

## Local image builds

Every image builds from a clean checkout with repo-root context:

```sh
docker build -f build/operator/Dockerfile -t tcdi/operator .
docker build -f build/api/Dockerfile       -t tcdi/api .
docker build -f build/gateway/Dockerfile   -t tcdi/gateway .
docker build -f build/portal/Dockerfile    -t tcdi/portal .   # builds web/ in-stage
docker build -f build/linux-desktop/Dockerfile -t tcdi/linux-desktop .
docker build -f build/browser/Dockerfile   -t tcdi/browser \
    --build-arg BASE_IMAGE=tcdi/linux-desktop .
```

The browser image layers the pinned browsers on top of the desktop image —
`BASE_IMAGE` selects which linux-desktop build to extend (a local tag or a
registry digest). See `docs/images.md` for the runtime-image contract and
pinned inputs.

## Regenerating the README screenshots

`web/scripts/screenshots.mjs` reproduces every image under
`docs/images/screenshots/` — real captures, no mockups. It is a manual
tool and is NOT wired into CI.

```sh
npm --prefix web ci          # once — provides Playwright + its browsers
node web/scripts/screenshots.mjs
```

What it does:

1. **Portal views** — boots the `tests-portal` harness: the built SPA
   served by the real Go portal binary over HTTPS with the contract mock
   (`web/tests/mock-api`) behind it, seeds generic demo workspaces through
   the mock's public + `/_control` endpoints, and captures the workspace
   list, create form, detail and retained-data pages.
2. **Live sessions** — runs `ghcr.io/tinyorbitvn/tinycdi-linux-desktop`
   and `ghcr.io/tinyorbitvn/tinycdi-browser` under Docker exactly like the
   integration suite (mounted TLS cert + credentials, tmpfs `/run/tcdi`,
   caps dropped, seccomp). The browser container additionally bind-mounts
   an xstartup that points Chromium at `https://example.com` instead of
   `about:blank` — the only deviation, the sandbox stays on. The KasmVNC
   web client is screenshotted over HTTPS with basic-auth credentials.
   Containers are named `tcdi-w19-*` and removed afterwards.

Requires Docker and ~5 GB of free disk for the runtime images.
Environment knobs: `SHOTS_DIR` (output directory), `DESKTOP_IMAGE` /
`BROWSER_IMAGE` (image refs), `KEEP=1` (leave containers running for
inspection).
