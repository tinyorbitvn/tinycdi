# Branding the portal

The portal can carry operator branding — product name, logos, favicons and
design-token overrides — without rebuilding the frontend image. Everything
is one ConfigMap mounted into the frontend pods and served same-origin at
`https://<portalHost>/branding/`.

Set `frontend.branding.configMap` to the ConfigMap's name (in the release
namespace). The chart mounts it read-only at `/branding` and starts the
frontend with `-branding-dir=/branding`. Empty (the default) renders no
mount and no flag — the portal keeps its shipped branding.

## ConfigMap layout

Every key is optional; only the keys you supply override the defaults.

| Key | Content |
|---|---|
| `branding.json` | `{"productName": "...", "logo": "/branding/<file>", "logoDark": "/branding/<file>"}` — the portal's product name and logo paths. Unknown fields are ignored |
| `tokens.css` | CSS custom-property overrides for the Orbit design tokens (`--tc-*`/`--to-*` defined in `web/src/design/design.css`), e.g. `:root { --tc-color-accent: #123456; }`. `index.html` links it **after** the app CSS, so the overrides win |
| logo files | e.g. `logo.svg` — any regular file referenced from `branding.json`; served with the extension's content type |
| `favicon.svg` / `favicon.ico` | replaces the shipped favicon at `/favicon.svg` / `/favicon.ico`. `favicon.ico` is binary — put it under the ConfigMap's `binaryData`, not `data` |

### `branding.json` semantics

`web/src/app/branding.ts` (`loadBranding`) fetches `/branding/branding.json`
at startup and validates **field by field** — an invalid field falls back
on its own, the file is never all-or-nothing:

- `productName` — non-empty string, else the default `"TinyCDI"`.
- `logo` / `logoDark` — must be a same-origin path starting with
  `/branding/`; absolute or scheme-relative URLs are rejected (a foreign
  logo URL could point at another origin). `logoDark` falls back to
  `logo`; an operator logo without a dark variant is used in both themes.
- A missing/unreadable `branding.json` (404, non-JSON, network error)
  yields the shipped defaults.

The serving contract is fail-safe for the browser console: with no
ConfigMap — or a ConfigMap missing the key — `/branding/branding.json`
answers `200 {}` and `/branding/tokens.css` an empty stylesheet, so neither
request is ever an error.

### Serving properties

`/branding/` serves regular files only: no directory listing,
`Cache-Control: no-cache`, and `..` path segments are rejected. A same-named
`favicon.svg`/`favicon.ico` in the branding directory replaces the shipped
file at the root URL (also `no-cache`).

## Worked example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: acme-branding
  namespace: tinycdi-system      # the release namespace
data:
  branding.json: |
    {"productName": "Acme Desktops",
     "logo": "/branding/logo.svg",
     "logoDark": "/branding/logo-dark.svg"}
  tokens.css: |
    :root {
      --tc-color-accent: #123456;
      --tc-color-accent-hover: #0e2a47;
    }
  logo.svg: |
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">…</svg>
  logo-dark.svg: |
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">…</svg>
binaryData:
  favicon.ico: <base64>
```

```yaml
# my-values.yaml
frontend:
  branding:
    configMap: acme-branding
```

## Applying changes

The frontend re-reads the files on every request, so an updated ConfigMap
propagates once the mounted volume refreshes (about a minute). Restart the
Deployment to apply immediately:

```bash
kubectl -n <release-ns> rollout restart deployment/frontend
```

No image rebuild is ever needed.

## Trademark note

The TinyOrbit name, wordmark and mark shipped as the default branding are
trademarks of TinyOrbit and are **not** covered by the MIT licence — see
[`TRADEMARKS.md`](../TRADEMARKS.md). Supplying your own `branding.json`
replaces the default product name and marks; the "TinyCDI by TinyOrbit"
attribution is shown only for the default branding. Fonts and icons
vendored under `web/src/design/orbit/` keep their own free licences (SIL
OFL-1.1, MIT) — see `THIRD_PARTY_LICENSES.md`.
