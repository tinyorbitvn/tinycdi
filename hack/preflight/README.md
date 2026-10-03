# TinyCDI preflight

`preflight.sh` checks a target cluster before you install the chart. Each
check prints `PASS`, `WARN` or `FAIL` with a one-line fix; the exit code is
`1` when any check `FAIL`s (`2` on a usage error). A check that cannot run
for lack of permissions or a missing local tool is a `WARN`, never a `FAIL`,
so the script works without cluster-admin.

```bash
hack/preflight/preflight.sh \
  --context my-cluster \
  --session-domain cdi.example.com \
  --tls-secret tinycdi-system/tinycdi-backend-session-tls \
  --oidc-issuer https://idp.example.com/realms/tinycdi \
  --postgres-dsn-secret tinycdi-system/tinycdi-backend-db:url \
  --postgres-ca-secret tinycdi-system/tinycdi-extra-ca:ca.crt \
  --host-users-false
```

Needs `kubectl` plus, for the matching checks, `openssl`, `curl`, `psql` and
`getent`. Override a binary with `KUBECTL`, `OPENSSL`, `CURL`, `PSQL`,
`GETENT`.

## Checks

| Check | PASS | WARN | FAIL |
|---|---|---|---|
| `k8s-version` | server ≥ `--min-k8s` (default 1.30) | `/version` not readable or not parseable | older, or API server unreachable |
| `netpol` | a deny-ingress policy blocks a probe connection that worked before it | no permission to create the probe namespace/pods/policy, `--no-probe`, probe image not pullable, baseline traffic broken | the policy did not block traffic: no CNI enforces NetworkPolicy |
| `storageclass` | a default class (or `--storage-class NAME`) exists | not permitted to list | none default / named class absent |
| `host-users` | not requested, or a `hostUsers: false` pod starts | probe could not run (no permission, `--no-probe`) | the pod is rejected or never starts |
| `node-pool` | nodes carry `--node-label` and the `--node-taint` key, or `--allow-shared-nodes` | labeled but untainted nodes; not permitted to list nodes | no node has the label |
| `dns` | `preflight-<random>.<sessionDomain>` resolves | no `--session-domain`, no `getent` | does not resolve |
| `tls-secret` | the certificate in the Secret covers `*.<sessionDomain>` and is valid for 14+ days | no ref given, not permitted to read, no `openssl`, expires within 14 days | missing Secret, wrong SANs, expired, unreadable PEM |
| `oidc` | `<issuer>/.well-known/openid-configuration` answers with the same `issuer` | no issuer given, no `curl`, plain `http` issuer | no answer, or the issuer differs |
| `postgres` | `select 1` works with `sslmode=verify-full` (or `verify-ca`) | no DSN source, no `psql`, host unreachable from here, `sslmode` that does not verify the server | TLS verification fails, server has no TLS, authentication fails |

`dns`, `oidc` and `postgres` run from the machine that runs the script, not
from inside the cluster. A cluster-internal database is usually unreachable
from a workstation: run preflight from a pod, or through
`kubectl port-forward` with a DSN that names the forwarded address.

## What it creates

Only a throwaway namespace `tcdi-preflight-<random>` (Pod Security
`restricted`) holding up to six short-lived probe pods and one deny-ingress
NetworkPolicy. The namespace is created lazily, only for `netpol` and
`host-users`, and an `EXIT`/`INT`/`TERM` trap deletes it, also after an
error. If the delete itself fails the script says so and exits `1`. With
`--no-probe` nothing is created and those two checks `WARN`. Everything else
is read-only (`get`, `auth can-i`). The probe pods run `--probe-image`
(default `registry.k8s.io/e2e-test-images/agnhost:2.53`); point it at a
mirror on an air-gapped cluster.

With `--host-users-false` the probe pod also runs on the workspace node pool
(label and toleration from `--node-label`/`--node-taint`) unless
`--allow-shared-nodes` is set, because that is where workspaces will run.

## Secrets

The DSN is read from a Secret (`--postgres-dsn-secret NS/NAME[:KEY]`, default
key `url`) or from the environment variable named by `--postgres-dsn-env`
(default `TCDI_PREFLIGHT_DSN`). It is held in memory, split into the `PGHOST`,
`PGPORT`, `PGUSER`, `PGDATABASE`, `PGPASSWORD`, `PGSSLMODE` (and `PGSSLROOTCERT`
etc.) variables, and handed to `psql` only through its environment, never as an
argument, so the password does not show up in the host's process list. Both the
URL form (percent-encode special characters in the password) and the
`key=value` form work; a parameter that has no `PG*` variable makes the check a
WARN rather than putting the DSN on the command line. It is never printed;
`psql` output is reduced to a fixed classification. The CA Secret and the
certificate are written to a `0700`
temporary directory that the trap removes. Only the `ca.crt`/`tls.crt` keys
are read.

## Flags and environment

| Flag | Environment | Default |
|---|---|---|
| `--context` | `TCDI_PREFLIGHT_CONTEXT` | current context |
| `--session-domain` | `TCDI_PREFLIGHT_SESSION_DOMAIN` | none |
| `--tls-secret NS/NAME[:KEY]` | `TCDI_PREFLIGHT_TLS_SECRET` | none (key `tls.crt`) |
| `--oidc-issuer` | `TCDI_PREFLIGHT_OIDC_ISSUER` | none |
| `--postgres-dsn-secret` | `TCDI_PREFLIGHT_DSN_SECRET` | none |
| `--postgres-dsn-env` | `TCDI_PREFLIGHT_DSN_ENV` | `TCDI_PREFLIGHT_DSN` |
| `--postgres-ca-secret` | `TCDI_PREFLIGHT_DB_CA_SECRET` | none (libpq default root cert) |
| `--node-label` | `TCDI_PREFLIGHT_NODE_LABEL` | `cdi.tinyorbit.vn/workspace=true` |
| `--node-taint` | `TCDI_PREFLIGHT_NODE_TAINT` | `cdi.tinyorbit.vn/workspace` |
| `--allow-shared-nodes` | | off |
| `--host-users-false` | | off |
| `--storage-class` | `TCDI_PREFLIGHT_STORAGE_CLASS` | any default class |
| `--min-k8s` | `TCDI_PREFLIGHT_MIN_K8S` | `1.30` |
| `--probe-image` | `TCDI_PREFLIGHT_PROBE_IMAGE` | see above |
| `--no-probe` | | off |

Timing knobs for slow clusters: `TCDI_PREFLIGHT_POD_TIMEOUT` (seconds to wait
for a probe pod, default 90), `TCDI_PREFLIGHT_PROPAGATE` (seconds the CNI gets
to program the policy, default 5), `TCDI_PREFLIGHT_POLL` (default 2).

## Tests

`.github/tests/preflight.test.sh` drives the script with `PATH` shims for
`kubectl`, `openssl`, `curl`, `psql` and `getent`, and covers every check in
PASS/WARN/FAIL, the exit code, namespace deletion on failure and on `SIGTERM`,
and that the DSN is never printed and the password never reaches `psql`'s
argument list (it arrives as `PGPASSWORD`).
