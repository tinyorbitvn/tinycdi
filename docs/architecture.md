# Architecture

The components of a TinyCDI install and where each lives in the repo.

## Components

- **Frontend** (`web/` + `build/frontend`): React SPA served as static
  files with browser security headers; it does not proxy `/v1` — the edge
  routes `/v1` to the backend.
- **Backend** (`cmd/backend`, `internal/backend`): one binary with four
  separate listeners, each with its own TLS config and route table —
  `:8443` app (the public REST surface `/v1`; `internal/api`:
  identity/OIDC, quotas, workspace CRUD, retained-data inventory),
  `:8444` session gateway on `*.<sessionDomain>` (`internal/gateway`:
  ticket redemption, launch-origin policy, authenticated reverse proxy
  to the runtime's streaming endpoint), `:9443` internal mTLS used by
  the operator only, and `:9090` metrics (optional).
- **Broker** (`internal/broker`): runs in-process in the backend —
  connection tickets (opaque, single-use, TTL-bound), leases, revocation,
  activity.
- **Operator** (`cmd/operator`, `internal/operator`): reconciles `Workspace`
  / `WorkspaceTemplate` CRDs (`api/`), provisions runtime pods, enforces
  per-workspace NetworkPolicy, runs the teardown finalizer.
- **State**: Postgres is the only state store — tickets, leases, quota,
  file-transfer grants and the audit log
  (`internal/store/migrations/0001_baseline.sql`).
- **Runtime images** (`build/linux-base`, `build/linux-desktop`,
  `build/browser`): non-root KasmVNC sessions on Debian bookworm. The
  pod's `:8443` is served by `tcdi-xferd` (`cmd/tcdi-xferd`), which
  serves `/xfer/files/{name}` and reverse-proxies everything else to
  KasmVNC on `127.0.0.1:8444` — see the
  [runtime contract](images.md#runtime-contract). `linux-base` is the
  shared runtime contract; `linux-desktop` (XFCE4) and `browser` (kiosk)
  are profiles built `FROM` it.
- **Kasm adapter** (`build/kasm-adapter`): runs cataloged `kasmweb/*`
  images behind the same contract — see [kasm-images.md](kasm-images.md).

Design decisions live in the ADRs under `docs/adr/`; pinned
dependency/toolchain versions are in `docs/compatibility.md`. Build, test
and repo-layout documentation is in `docs/development.md`.
