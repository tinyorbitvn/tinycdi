# Soak and drill harness

Tools for the v0.2 acceptance criteria that need sustained load and failure
injection (criteria 2, 3 and 9 — see the release plan). Nothing here runs in
CI against a cluster; `drills.sh` is meant to be launched by the operator
running the RKE2 drill, and `soak.ts` runs anywhere that can reach the
portal.

## Layout

| File | Purpose |
|---|---|
| `soak.ts` | Playwright soak runner: login, launch N sessions, input ticks, mid-run reload, report |
| `drills.sh` | `delete-pod` / `rollout` / `rotate-cert` drills with UTC timestamps |
| `report.schema.json` | JSON Schema (draft 2020-12) for `soak-report.json` |
| `metrics.ts` | percentile, gap and duration math (pure, unit-tested) |
| `report.ts` | report assembly + schema validation (ajv) |
| `soak.test.ts` | `node --test` metrics/report unit tests and the dry-run end-to-end check |
| `drive.test.ts` | orchestration tests against a scripted fake driver (relaunch, soak clock, reload, SIGINT, option validation) |
| `drills.test.ts` | `drills.sh` tests against a fake `kubectl` |

The package has its own lockfile on purpose: it is a standalone harness, not
part of `web/`.

## soak.ts

```sh
npm ci
# real run (needs `npx playwright install chromium` once):
SOAK_PORTAL_URL=https://portal.example.com \
SOAK_USER=alice SOAK_PASSWORD=… \
SOAK_SESSIONS=25 SOAK_DURATION=60m \
npm run soak -- --connect-p95-ms 30000 --reconnect-p95-ms 15000 --max-gap-ms 60000

# dry run against the contract mock API (no cluster, no browser):
npm run test:dry-run
```

What a run does, in order:

1. Logs in. Real runs drive a real OIDC login in Chromium: the browser is
   sent to `SOAK_PORTAL_URL`, follows the redirect to the identity provider,
   fills the first username/email field and the password field, and submits.
   Override the field pickers with `SOAK_USER_SELECTOR`,
   `SOAK_PASSWORD_SELECTOR`, `SOAK_SUBMIT_SELECTOR` when the IdP form is
   unusual. Dry runs use the mock API's dev `POST /v1/login`.
2. Resolves `SOAK_TEMPLATE` (id or name) against `GET /v1/templates`.
3. Creates `SOAK_SESSIONS` workspaces (`desiredState: Running`) with
   `Idempotency-Key` + CSRF (`/v1/me` csrfToken, falling back to a legacy
   `tcdi_csrf` cookie), waits for `Ready`, then opens each session: real
   runs navigate a tab to `/workspaces/{id}/session` (the in-portal view
   fetches a `LaunchTicket` and POSTs it into the sandboxed iframe); dry
   runs do the same flow at request level against the session origin.
4. Each session starts being polled right after it is opened, so `connectMs`
   is per session. The soak clock starts once the last session has first
   connected (workspace creation and readiness do not count against it), and
   for `SOAK_DURATION` after that every session is polled on
   `GET /v1/workspaces/{id}/connection` (the T2.3 `ConnectionStatus`) every
   `SOAK_POLL_INTERVAL` (default 5 s) and gets scripted mouse+keyboard input
   every `SOAK_INPUT_INTERVAL` (default 10 s). Where the connection endpoint
   does not exist (the pre-T2.5 mock) the runner falls back to probing the
   desktop URL on the session origin and marks those rows `"probe"`.
5. At half the soak duration every session page is reloaded once. The reload
   resumes the session without a ticket, so a take-over prompt after it is a
   harness failure (the run fails and counts a manual action). The time from
   the first non-connected observation after the reload to the next
   `connected` one is the reconnect measurement, but only a non-connected
   observation within 60 s after the reload counts; a reload without one is
   seamless (`reconnectMs: 0`, `seamless: true`) and a later drill gap is never
   attributed to it.
6. At the end — and on failure or SIGINT — every workspace it created is
   stopped/deleted before the report is written.

A session that never reaches `connected`, or stays non-connected past
`SOAK_CONNECT_TIMEOUT` (default 120 s), is re-launched automatically at most
`MAX_AUTO_RELAUNCH` = 2 times per 5 minutes (same bound as the portal's
`useConnectionWatch`). Every relaunch gets the full connect budget again (the
stale browser tab is closed first); a relaunch that then connects is not a
drop. Past the bound the session is recorded as `dropped` plus one
`manualActions` — the report counts sessions that needed a human.

A run that ends before the requested duration has elapsed on the soak clock
(SIGINT, abort) is **truncated** and fails; so does a run whose soak clock
never started. Numeric flags and `SOAK_SESSIONS` are validated up front.
Playwright's own signal handlers are disabled, so SIGINT/SIGTERM/SIGHUP
always run the cleanup and write the report.

### Report

`soak-report.json` is validated against `report.schema.json` before it is
written. Per session it records `connectMs` (launch → first `connected`),
`reconnectMs` (first non-connected observation within 60 s after the reload → `connected`; 0 when `seamless`), `longestGapMs` (longest continuous
non-connected stretch after the first connect), every span spent in a state
other than `connected`, `manualActions`, `inputEvents` and `dropped`. The
summary carries p50/p95 of connect and reconnect (nearest-rank), the manual
action and drop counts, and `pass`/`failures` against the `--connect-p95-ms`,
`--reconnect-p95-ms`, `--max-gap-ms`, `--max-manual-actions` and
`--max-dropped` thresholds. Exit code is 0 on pass, 1 on fail.

Credentials are read from the environment only, are never logged and never
reach the report; `run.portalOrigin` stores the origin only.

## drills.sh

```sh
tests/soak/drills.sh delete-pod                # delete one backend pod, wait for the Deployment
tests/soak/drills.sh rollout                   # rollout restart deployment/backend
tests/soak/drills.sh rotate-cert --certificate tinycdi-session-tls
tests/soak/drills.sh rotate-cert --secret tinycdi-session-tls
tests/soak/drills.sh -n tinycdi rollout        # options go before or after the drill name
```

Every drill prints a UTC timestamp before and after it acts — line the
`nonConnectedStates` spans in the report up with those windows. Knobs:
`SOAK_NAMESPACE` (default `tinycdi`), `SOAK_DEPLOYMENT` (`backend`),
`SOAK_SELECTOR` (`app.kubernetes.io/name=backend`), `KUBECTL`.

`rotate-cert` runs `cmctl renew` / `kubectl cert-manager renew` on a
cert-manager `Certificate` (`--certificate`, or `--secret` whose owning
Certificate is looked up), which re-issues the key material and exercises
the TLS hot-reload path (D21). A Secret that no Certificate owns cannot be
rotated from here and the drill fails — re-issue it with your PKI. An
unknown option is an error.

## Development

```sh
npm ci            # installs this package only
npm test          # unit tests + the 25 s dry-run e2e against the mock API
npm run typecheck
npm run lint:sh   # shellcheck drills.sh
```
