# ADR 0009 — Per-certificate revocation on the internal mTLS API (SR-3-F5)

Status: **proposed** (design only — implementation target v1.1 if merged
before the v1.1.0-rc.1 cut, else v1.2)

Date: 2026-10-11

## Context

The internal broker API (ADR 0003) is served from the backend's
`-internal-listen` (`:9443`) over mTLS: `VerifyClientCertIfGiven` against
the mounted client-CA bundle, TLS 1.3, with the client identity mapping
to either the gateway CN or the operator CN
(`internal/broker/httpapi/server.go` `ServerTLSConfig`/`identify`). Two
client populations exist: the operator (`opclient`, workspace
revoke/drain) and, in split mode, the session gateway (`brokerclient`,
redeem/renew/target/revoke/activity).

Certs, the server keypair and the client-CA bundle already hot-reload
(`tlsreload.Reloader`/`CAPool`, `hotReloadClientCAs`; threat-model S15
held at v1.0): rotating the CA or a leaf takes effect without a restart.
What does not exist is any way to distrust **one** certificate while its
CA keeps being trusted — no CRL, no OCSP, no serial blocklist. Revoking
a single compromised operator or gateway cert today means rotating the
client-CA bundle, which drops every client of the listener at once
(SR-3-F5, accepted residual, owner `maintainers`, target v1.1).

The reviewer offered three shapes: (i) a serial blocklist, (ii) a CRL,
(iii) shorter certificate lifetimes.

## Decision

Adopt **(i) + (iii)**: a per-serial blocklist enforced at the TLS
handshake of every internal mTLS server, plus documented shorter
certificate lifetimes. Reject **(ii) CRL** — rationale below.

### (i) Serial blocklist

#### Blocklist format

A plain-text file, one revoked certificate per line:

```
# one entry per line; blank lines and lines whose first
# non-whitespace character is '#' are ignored. No trailing comments.
<SERIAL-HEX> <ISSUER-DN>
4f8b2c1a09d3e4f5 CN=tinycdi-internal-ca
```

- `SERIAL-HEX` — hex of the certificate's serial number, exactly as
  `openssl x509 -noout -serial` prints it after `serial=`: colons are
  stripped, case-insensitive, no `0x` prefix, leading-zero octets
  optional (entries compare as big integers — `serial.Text(16)` — so
  `04ab` and `4ab` denote the same serial).
- `ISSUER-DN` — the rest of the line, trimmed. Compared byte-for-byte
  against the presented leaf's `cert.Issuer.String()` (Go `pkix.Name`
  RFC 2253 form). For the chart-issued self-signed CA this is exactly
  `CN=tinycdi-internal-ca`; `openssl x509 -noout -issuer -nameopt
  RFC2253` prints the same string after `issuer=` for ordinary DN
  shapes. The issuer half makes the match an (issuer, serial) pair —
  serial numbers are only unique per issuer, and the client-CA bundle
  may legitimately carry several CAs during a rotation.
- Parsing is **atomic**: any malformed line rejects the whole file. A
  partially applied blocklist is worse than a stale one.
- Bounds: files larger than 1 MiB or carrying more than 8192 entries
  are invalid (a blocklist is small by nature; the bound stops a
  bad push from stalling the handshake path).

#### File path and reload mechanism

- New backend flag `-internal-revoked-list` (env
  `TCDI_INTERNAL_REVOKED_LIST`), empty by default → feature off and the
  wire is byte-identical to today.
- The chart mounts it from a Secret (values below); the pods keep
  consuming plain secret volumes, same as the TLS material.
- **Reload: poll, not fsnotify.** A `tlsreload.Blocklist` mirrors
  `CAPool` exactly: re-read on a 30 s interval, swap on content-hash
  change. fsnotify was considered and rejected — Kubernetes Secret
  volumes update by `..data` symlink swap, which invalidates watch
  registrations and leaves a silent stale list (a fail-open window the
  operator cannot observe); hash-polling has no missed-event mode,
  matches the mechanism already proven for cert/CA reload (E5/S15), and
  makes staleness explicit (poll interval + the kubelet's Secret sync,
  ~90 s worst case).
- Failure modes, mirroring `CAPool`:
  - **Boot**: flag set + file unreadable/invalid → startup fails
    (`NewBlocklist` errors like `NewCAPool`). A configured revocation
    control never starts silently disabled — that is the fail-open
    case this design exists to remove.
  - **Reload**: unreadable or invalid file → keep the last-good list,
    increment
    `tinycdi_internal_mtls_revocation_reload_errors_total`, WARN
    `tlsreload: revocation list reload failed, keeping previous list`.
    Never fail open silently, never crash-loop.
- The empty list is a valid list: an operator can shrink the file to
  zero entries deliberately. "No file" and "empty file" are different
  states — the former is an error, the latter revokes nothing.

#### Enforcement point

`tls.Config.VerifyConnection` on the server side — installed inside
`hotReloadClientCAs` (`internal/backend/listeners.go`), which already
builds the per-handshake cloned config with the current `ClientCAs`
pool. The clone additionally consults `Blocklist.Current()`: if the
verified peer leaf's (issuer, serial) is listed, the callback returns an
error and the handshake aborts with a TLS alert.

- Coverage is **structural**: every internal mTLS server built through
  `hotReloadClientCAs` inherits the check. Today that is exactly the
  `-internal-listen` `:9443` listener — the metrics listener is
  plaintext scrape-only, the app/session listeners verify no client
  certs, and the operator's `:9443` admission webhook serves the
  Kubernetes apiserver (TLS but not this mTLS hop). Any future internal
  mTLS server gets the control by construction rather than by
  remembering to wire it.
- A revoked cert therefore gets a **handshake abort**, not the
  listener's JSON 401 — `VerifyConnection` runs before any HTTP
  request exists. The client sees a transport error (`bad certificate`
  alert); `opclient` surfaces it as `TRANSPORT`. Fail-closed, and the
  401 error model for *absent* certs is unchanged.
- Revocation binds **new handshakes**: an established keep-alive
  connection keeps serving until it closes (`IdleTimeout` 120 s bounds
  this on the internal listener). Acceptable — the lease/fence layer
  below is the recourse for a still-connected caller, exactly as when
  a CA rotates mid-connection today.
- **Client side: optional.** `opclient`/`brokerclient` may apply the
  same (issuer, serial) check to the *server* leaf inside their
  existing `VerifyConnection`, reading the same file when configured.
  Not wired by default: a compromised server cert means the internal CA
  itself is suspect — rotate the CA, do not blocklist it.

#### Metrics and logging on a rejected serial

- `tinycdi_internal_mtls_revoked_serial_total{listener="internal"}` —
  counter, one per rejected handshake; `listener` is a bounded label
  (`internal` today) so future mTLS servers slot in.
- `tinycdi_internal_mtls_revocation_entries` — gauge, entries in the
  currently loaded list (absent/zero when the feature is off).
- `tinycdi_internal_mtls_revocation_reload_errors_total` — counter,
  one per failed reload (kept-last-good events).
- WARN log `internal_mtls_revoked_cert`, rate-limited with the same
  token-budget shape as `rejectAuth` (1/s sustained, burst 10, keyed
  `revoked_serial`): fields `peer`, `serial`, `issuer`. The WARN
  doubles as the forensic source for the exact `ISSUER-DN` string form
  when an exotic DN does not round-trip through openssl's RFC 2253
  output. Certificate, key and SAN material are never logged; there is
  no request path or request id at handshake time (SEC-41 unaffected).

### (ii) CRL — rejected

A real CRL path needs generation, signing, publication, refresh and
staleness policy end-to-end — none of which exists: cert-manager's
`ca`/`selfSigned` issuers (what the chart bootstraps) publish **no CRL**
and set no CRL distribution point on issued certs, so every deployment
on the shipped chain has nothing to fetch. Standing up a CRL signer +
distribution endpoint + server-side refresh loop is a new subsystem to
fix a P3, and its failure modes (stale CRL accepted? fetch outage? —
the same questions this ADR answers for a file, minus the file's
simplicity) land strictly worse than the mounted-Secret blocklist,
which rides the already-proven `tlsreload` path. Revisit only if the
internal PKI moves to an issuer that publishes CRLs natively.

### (iii) Shorter certificate lifetimes — documented

Rotation already works end-to-end (S15), so lifetime is the exposure
window when revocation has not been applied. The chart defaults stay
`certManager.duration: 2160h` / `renewBefore: 360h` — changing shipped
defaults rewrites issued certs for every existing `certManager.enabled`
deployment and is a deliberate values-visible change, not something to
ride invisibly on this fix. The ADR's documented recommendation for the
internal chain (set explicitly in values or the equivalent on an
external issuer):

```yaml
certManager:
  duration: 168h      # 7d leaf lifetime
  renewBefore: 24h    # renew a day early; cert-manager requires < duration
```

That bounds the un-revocable window to a week worst case instead of 90
days, at ~13 rotations per quarter per cert — cheap against the
existing hot-reload path, and the blocklist covers the
compromised-before-expiry residual inside it.

## Chart values (additive)

```yaml
backend:
  tls:
    internal:
      existingSecret: tinycdi-backend-internal-tls
      revokedSerials:               # NEW — empty = feature off
        existingSecret: ""          # Secret carrying the blocklist file
        key: revoked-serials.txt    # key within the Secret
```

When `existingSecret` is set the render adds a read-only Secret volume
mounted at `/etc/internal-revoked/` (the key projected to a fixed
`revoked-serials.txt` filename, mirroring the `client-ca` projection
pattern) and appends
`-internal-revoked-list=/etc/internal-revoked/revoked-serials.txt` to
the backend args. No existing value is renamed or removed; default
renders are byte-identical (`TestDefaultRenderUnchangedGolden` keeps
passing).

## Operator runbook — revoke one certificate

1. **Get serial + issuer** of the cert to revoke:

   ```sh
   kubectl -n <ns> get secret tinycdi-operator-mtls \
     -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/cert.pem
   openssl x509 -in /tmp/cert.pem -noout -serial
   openssl x509 -in /tmp/cert.pem -noout -issuer -nameopt RFC2253
   ```

   → e.g. `serial=4F:8B:2C:1A`, `issuer=CN=tinycdi-internal-ca`.
2. **Append** `<serial-hex> <issuer-dn>` to the blocklist file in the
   `revokedSerials` Secret (`kubectl edit secret`, `kubectl create
   secret --from-file` over a GitOps-managed file, etc.).
3. **Wait** for the kubelet Secret sync plus one 30 s poll (~90 s worst
   case; a rollout restart forces immediate pickup).
4. **Verify rejected**:

   ```sh
   openssl s_client -connect backend.<ns>.svc:9443 \
     -cert /tmp/cert.pem -key /tmp/key.pem -CAfile /tmp/ca.crt
   ```

   → handshake alert; confirm
   `tinycdi_internal_mtls_revoked_serial_total` incremented and the
   `internal_mtls_revoked_cert` WARN line names the serial.
5. **Reissue** a replacement cert (`cmctl renew <release>-operator-mtls`
   with cert-manager, or the external issuer's flow); the
   CAPool/Reloader path picks it up on the next handshake.
6. **Keep the entry.** The list is append-only for audit; entries are
   cheap. On a client-CA rotation the whole list becomes moot (issuers
   change) — reset the Secret then.

## Test plan

- **Unit** (`internal/tlsreload`, `internal/backend`):
  - blocked serial rejected at the handshake; sibling certs from the
    same CA still accepted;
  - same serial under a different issuer does **not** match
    ((issuer, serial) pairing, CA-rotation overlap);
  - reload picks up an appended entry without restart;
  - malformed line / unreadable file / deleted file → keeps last good,
    reload-error counter increments, WARN logged, no crash-loop;
  - flag set + bad file at boot → startup fails;
  - hex parsing: colons, uppercase, leading-zero octet all normalize;
  - flag unset → byte-identical behaviour (no `VerifyConnection`
    installed).
- **Integration**: extend the existing internal-listener stack
  (`internal/backend/mtls_reload_test.go` `serveInternalTLS`/`callMTLS`)
  with a revoked-cert leg; wire one revoked-serial leg into the
  internal-API drill family that already covers `:9443` auth failures
  (the SR-3-F3/SR-3-F4 tests).
- **Chart**: default render unchanged (golden), set-values render adds
  volume/mount/arg.

## Compatibility

- `/v1` HTTP API untouched — the change is at the TLS layer of the
  internal listener; no route, body or error-model change (revoked
  certs die before HTTP exists).
- Chart values are additive with safe defaults (`""` = off);
  `certManager.duration`/`renewBefore` defaults unchanged.
- Existing un-revoked clients see zero behavioural change; the feature
  only ever shrinks the accepted set when explicitly configured.

## Rollout

Docs only — this ADR is the design. Implement in **v1.1** if this ADR
is merged before the `v1.1.0-rc.1` cut (implementation then adds the
flag, `tlsreload.Blocklist`, the `VerifyConnection` hook, metrics, the
chart values, the tests above, and a `docs/releases/v1.1.0.md` note per
the versioning contract). If the ADR lands after the cut, the
implementation waits for **v1.2**; SR-3-F5's accepted residual stands
unchanged in the interim (rotation remains the revocation mechanism).

## Consequences

- `docs/README.md` ADR list gains this entry; threat-model S15's status
  line points here.
- One new backend flag + env, one small `tlsreload` type, three metric
  series, two chart values, one Secret convention.
- The v1.0 residual narrows from "rotate the CA to revoke one cert" to
  "revocation takes up to ~90 s to propagate and does not sever an
  already-established connection" — both documented above.
