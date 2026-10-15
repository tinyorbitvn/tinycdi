# Contributing

## CI checks

Every pull request runs the Go, web, chart and workflow checks in
`.github/workflows/ci.yml` (see `.github/README.md` for the pipeline's
supply-chain layout). Three cluster-level jobs are gated on the paths they
exercise and are **not** required checks — they run on `main` pushes,
`workflow_dispatch`, and PRs touching their surface:

- **`quickstart`** — `hack/quickstart/up.sh` brings up kind + Traefik +
  Postgres + dev Keycloak, installs the chart from the working tree and a
  Playwright smoke (`hack/quickstart/smoke/smoke.spec.ts`) logs in and
  round-trips a workspace session. Runs for `hack/quickstart/`,
  `deploy/helm/`, `build/`.
- **`kasm-contract`** — the Kasm adapter pod contract plus the catalog
  trivy/freshness gate; weekly, and for the kasm surfaces listed in the
  job's gate.

The scripts are dev tools too: `hack/quickstart/up.sh` /
`down.sh` run against any local
Docker daemon (`docs/quickstart.md`).
