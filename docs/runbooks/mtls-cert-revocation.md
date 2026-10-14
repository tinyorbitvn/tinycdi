# Revoking one internal mTLS certificate (ADR 0008)

The internal listener (`:9443`) verifies every client cert against the
mounted client-CA bundle. To distrust ONE certificate while its CA stays
trusted — a compromised operator or gateway client cert — the backend
supports a per-certificate serial blocklist, enabled by the chart value
`backend.tls.internal.revokedSerials.existingSecret` (backend flag
`-internal-revoked-list`). With the value unset the feature is off and
nothing changes.

Enforcement: a leaf whose `(issuer, serial)` pair is listed fails the TLS
handshake — the client sees a transport error (`alert bad certificate`),
not the listener's 401 — and live connections on a newly blocked serial
are severed on the next list reload. Propagation is the kubelet Secret
sync plus one 30 s poll — ~90 s worst case.

## List format

One revoked certificate per line — `<serial-hex> <issuer-dn>`:

```
# one entry per line; blank lines and lines whose first
# non-whitespace character is '#' are ignored. No trailing comments.
<SERIAL-HEX> <ISSUER-DN>
4f8b2c1a09d3e4f5 CN=tinycdi-internal-ca
```

- `SERIAL-HEX` — the cert's serial as `openssl x509 -noout -serial`
  prints it after `serial=`: colons stripped, case-insensitive, no `0x`
  prefix, leading-zero octets optional (entries compare as big integers,
  so `04ab` and `4ab` are the same serial).
- `ISSUER-DN` — the rest of the line, trimmed. For the chart-issued
  self-signed CA this is exactly `CN=tinycdi-internal-ca`; in general it
  must equal `openssl x509 -noout -issuer -nameopt RFC2253` output after
  `issuer=`. The match is an (issuer, serial) pair — serials are only
  unique per issuer.
- The file is parsed atomically: one malformed line rejects the whole
  file; the backend then keeps the last-good list (never applies a
  partial list). Files over 1 MiB or over 8192 entries are invalid.

## Revoke one certificate

1. **Get serial + issuer** of the cert to revoke (and keep the key + CA
   for the verify step):

   ```sh
   kubectl -n <ns> get secret tinycdi-operator-mtls \
     -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/cert.pem
   kubectl -n <ns> get secret tinycdi-operator-mtls \
     -o jsonpath='{.data.tls\.key}' | base64 -d > /tmp/key.pem
   kubectl -n <ns> get secret tinycdi-internal-ca \
     -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/ca.crt
   openssl x509 -in /tmp/cert.pem -noout -serial
   openssl x509 -in /tmp/cert.pem -noout -issuer -nameopt RFC2253
   ```

   → e.g. `serial=4F:8B:2C:1A`, `issuer=CN=tinycdi-internal-ca`.
2. **Append** `<serial-hex> <issuer-dn>` to the blocklist file in the
   `revokedSerials` Secret (`kubectl edit secret`, `kubectl create
   secret --from-file` over a GitOps-managed file, etc.).
3. **Wait** for the kubelet Secret sync plus one 30 s poll (~90 s worst
   case; a rollout restart forces immediate pickup).
4. **Verify rejected.** `backend.<ns>.svc:9443` only resolves
   in-cluster; two ways to reach it —
   - port-forward from any operator workstation and run `s_client`
     locally (nothing needs to exist inside the image):

     ```sh
     kubectl -n <ns> port-forward svc/backend 9443:9443 &
     openssl s_client -connect localhost:9443 \
       -cert /tmp/cert.pem -key /tmp/key.pem -CAfile /tmp/ca.crt
     ```

   - or run `s_client` inside a throwaway pod in the namespace with
     the `mtls`/`broker-ca` Secrets mounted (the operator pod's own
     mounts, before step 5 reissues them).

   → expected: `alert bad certificate` / handshake failure; confirm
   `tinycdi_internal_mtls_revoked_serial_total` incremented and the
   `internal_mtls_revoked_cert` WARN line names the serial. The WARN
   line is also the forensic source for the exact `ISSUER-DN` string
   form when an exotic DN does not round-trip through openssl's
   RFC 2253 output.
5. **Reissue** a replacement cert (`cmctl renew <release>-operator-mtls`
   with cert-manager, or the external issuer's flow); the
   CAPool/Reloader path picks it up on the next handshake.
6. **Keep the entry.** The list is append-only for audit; entries are
   cheap. On a client-CA rotation the whole list becomes moot (issuers
   change) — reset the Secret then.

## Failure modes

- **Boot**: flag set + file unreadable or invalid → the backend refuses
  to start. A configured revocation control never starts silently
  disabled.
- **Reload**: an unreadable/invalid file keeps the last-good list,
  increments `tinycdi_internal_mtls_revocation_reload_errors_total` and
  logs `tlsreload: revocation list reload failed, keeping previous list`
  at ERROR. A deleted file is a failed reload, not an empty list — an
  empty file IS a valid list that revokes nothing.
- Gauge `tinycdi_internal_mtls_revocation_entries` reports the loaded
  entry count (0 when the feature is off).

## Shrinking the exposure window

Revocation covers certs compromised before expiry; lifetime bounds the
window when it has not been applied. ADR 0008's documented
recommendation for the internal chain (set explicitly — shipped defaults
are unchanged):

```yaml
certManager:
  duration: 168h      # 7d leaf lifetime
  renewBefore: 24h    # renew a day early; cert-manager requires < duration
```

If the internal CA itself is compromised, blocklisting is NOT the fix —
rotate the CA (rotation drops every client at once, which is correct for
a suspect CA).
