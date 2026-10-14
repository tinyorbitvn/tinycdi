# ADR 0008 — Per-certificate revocation on the internal mTLS API

Status: Accepted

## Context

The internal broker API (ADR 0003) is served from the backend's
`-internal-listen` (`:9443`) over mTLS — `VerifyClientCertIfGiven`
against the mounted client-CA bundle, TLS 1.3, the client identity
mapping to either the gateway CN or the operator CN — and a single
compromised client certificate must be distrusted without rotating its
CA (SR-3-F5).

Certs, the server keypair and the client-CA bundle already hot-reload
(`tlsreload.Reloader`/`CAPool`, `hotReloadClientCAs`): rotating the CA
or a leaf takes effect without a restart. But CA rotation drops every
client of the listener at once, so it is the wrong tool for one stolen
or lost operator or gateway certificate.

## Decision

`-internal-revoked-list` (env `TCDI_INTERNAL_REVOKED_LIST`,
`internal/backend/config.go:350`) names a plain-text file of revoked
certificates, one per line:

```
# blank lines and lines whose first non-whitespace character is '#'
# are ignored; no trailing comments.
<SERIAL-HEX> <ISSUER-DN>
4f8b2c1a09d3e4f5 CN=tinycdi-internal-ca
```

- `SERIAL-HEX` — hex of the certificate's serial number as
  `openssl x509 -noout -serial` prints it after `serial=`: colons
  stripped, case-insensitive, no `0x` prefix; entries compare as big
  integers, so leading-zero octets are optional.
- `ISSUER-DN` — the rest of the line, trimmed, compared byte-for-byte
  against the presented leaf's `cert.Issuer.String()` (RFC 2253); for
  the chart-issued CA this is exactly `CN=tinycdi-internal-ca`. The
  issuer half makes the match an (issuer, serial) pair — serial numbers
  are unique only per issuer, and the client-CA bundle may carry
  several CAs during a rotation.
- Parsing is atomic — any malformed line rejects the whole file — and
  bounded (1 MiB, 8192 entries).

The backend polls the file and swaps the parsed list in exactly like
the client-CA pool: `tlsreload.Blocklist` mirrors `CAPool`, reloading on
a content-hash poll rather than fsnotify because Kubernetes Secret
volumes update by `..data` symlink swap, which silently defeats watch
registrations. A bad file at boot fails startup — a configured
revocation control never starts silently disabled — and a bad reload
keeps the last-good list (`internal/backend/wire.go:497-519`). An empty
file is a valid list that revokes nothing; "no file" is the error.

Enforcement is `tls.Config.VerifyConnection` on the server side,
installed by `installRevocation`
(`internal/backend/listeners.go:120`) inside the per-handshake config
clone `hotReloadClientCAs` already builds: a verified peer leaf whose
(issuer, serial) is listed aborts the handshake with a TLS alert — a
revoked certificate dies before any HTTP request exists, so the 401
model for absent certs is unchanged. `Blocklist` additionally keeps a
live-connection registry (`Track`/`Drop`,
`internal/tlsreload/blocklist.go:74-90`): a reload that adds an entry
closes the live connections already using that serial, because a
keep-alive caller could otherwise hold a revoked connection open
indefinitely (`http.Server` has no max connection lifetime). With the
flag unset no hook is installed and the handshake path is
byte-identical.

Chart: `backend.tls.internal.revokedSerials.existingSecret` names the
Secret holding the blocklist file and `.key` the key within it (default
`revoked-serials.txt`); the render mounts it read-only at
`/etc/internal-revoked/` and appends the flag
(`deploy/helm/tinycdi/values.yaml:298-306`). Metrics:
`tinycdi_internal_mtls_revoked_serial_total{listener="internal"}`
counts aborted handshakes, `tinycdi_internal_mtls_revocation_entries`
gauges the loaded entries and
`tinycdi_internal_mtls_revocation_reload_errors_total` counts
kept-last-good reloads (`internal/observability/metrics.go`); a
rate-limited WARN `internal_mtls_revoked_cert` names the peer, serial
and issuer of each refusal.

Certificate lifetimes bound the un-revocable window: cert-manager
`certManager.duration` (default `2160h`, 90 days) and
`certManager.renewBefore` (`360h`)
(`deploy/helm/tinycdi/values.yaml:542-543`). An operator may shorten
them; the blocklist covers a stolen key until its certificate expires.

The operator procedure — read the serial and issuer off the
certificate, append to the Secret, verify the refused handshake,
reissue — is
[docs/runbooks/mtls-cert-revocation.md](../runbooks/mtls-cert-revocation.md).

### Tests

- `internal/tlsreload` (`blocklist_test.go`): `TestParseBlocklistFormat`,
  `TestParseBlocklistRejectsWholeFile`, `TestParseBlocklistBounds`,
  `TestNewBlocklistFailsClosed`, `TestBlocklistReloadKeepsLastGood`,
  `TestBlocklistReloadSwaps`, `TestBlocklistTrackAndSever`,
  `TestBlocklistConcurrentTrackAndSwap`.
- `internal/backend` (`mtls_revocation_test.go`):
  `TestRevokedSerialHandshakeAborts`, `TestRevokedSerialIssuerPairing`,
  `TestRevokedListReloadBlocks`, `TestRevokedResumedHandshakeRejected`,
  `TestRevokedLiveConnSevered`, `TestRevocationOffByteIdentical`,
  `TestRevocationAbsentCertStill401`, `TestRevokedSerialWarnAndMetric`,
  `TestRevokedListReloadErrorMetric`, `TestParseFlags_RevokedList`.
- Chart (`deploy/helm/chart_revocation_test.go`):
  `TestRevokedSerialsRenderOff`, `TestRevokedSerialsRenderOn`,
  `TestRevokedSerialsCustomKey`.

## Alternatives considered

- **A CRL.** Rejected: the chart's cert-manager `ca`/`selfSigned`
  issuers publish no CRL and set no distribution point, so a CRL needs
  a new signing and publishing subsystem.
- **OCSP.** Rejected for the same reason: the shipped PKI runs no
  responder.

## Consequences

- Revocation propagates within one poll interval plus the kubelet
  Secret sync (~90 s worst case), then severs both new handshakes and
  established connections on the listed serial.
- One backend flag + env, one `tlsreload` type, three metric series,
  two chart values, one Secret convention.
- Stated limitation: the internal mTLS **clients** — the operator's
  broker client (`internal/broker/httpapi/opclient/client.go:120-130`)
  and the gateway's `brokerclient` — verify the server chain but do not
  check the server leaf's serial against the list. A compromised server
  certificate means the internal CA itself is suspect: rotate the CA.
