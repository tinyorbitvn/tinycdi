# Vendored Orbit System 2.9

Provenance: **TinyOrbit Orbit System 2.9**, exported 2026-10-02.

The stylesheets in `css/` are the eight Orbit source stylesheets, vendored
byte-for-byte except `fonts.css`, whose `url()` values were rewritten from
`../assets/fonts/` to `../fonts/` to match this layout. Load order is binding
(see `web/src/styles/index.css`): `fonts`, `tokens`, `expressive-tokens`,
`extended-colors`, `companion-palettes`, application CSS,
`component-language`, `navy-night`, and `palette-system` always last.

Fonts in `fonts/` are the WOFF2 subsets `fonts.css` references, taken from
the Fontsource npm packages (SIL Open Font License 1.1):

- `@fontsource-variable/manrope@5.3.0` — Manrope Variable (latin, latin-ext,
  vietnamese)
- `@fontsource/jetbrains-mono@5.3.0` — JetBrains Mono 400 (latin, latin-ext,
  vietnamese)

Icons the portal uses are Tabler Icons 3.48.0 outline (`@tabler/icons`,
MIT); their license text is in `LICENSES/`.

SHA-256 of every vendored file (verified by
`web/tests/unit/design/tokens.test.ts` — edit a stylesheet without updating
this manifest and the test fails):

```
f32a3a4e8e10c9273e2b038004721be514eb79e89d56d3f4f4684d0ba17dadbd  css/companion-palettes.css
75b8013982f50e922eaa0d63cbf246bb0d34b99ee4c1e166cf74595c85d67f0e  css/component-language.css
9d64b3c19e986d8f540b9d7346380ed7b856a04d23f752b3f02b3a4b81ff7eb5  css/expressive-tokens.css
61ffabe95495ff463168b99ff2e90b0731e2364ed210bc9a5a75ce12f641a029  css/extended-colors.css
c55eba5b2fa52441739015ba17deb1a9485996797599b620e85ca7644fa3ddeb  css/fonts.css
0d91514e13b52c30237527e638d37d425b28e0ca9441b3ed97b5af3fce4dfcc2  css/navy-night.css
deb4aaab82c3fd8ab57bd9382eff95e3c4fb0c9affa34483a4b7896e17c4e0b3  css/palette-system.css
0ff3165b06f59e03acf20ae058901227de3e8e4090a9ea4998fe8558db699166  css/tokens.css
14425ba9c695763c1547f48a206b7aa60350a33ae23de09f0407877f3fcd89eb  fonts/jetbrains-mono-latin-400-normal.woff2
505dfba8ecbe77e82765f36d317ed7ef4ac42719dc5f4ae68d1c483fd22d0d14  fonts/jetbrains-mono-latin-ext-400-normal.woff2
efc9e0df126ca6dd1c37a022bfe1d664d23b3fcaa32979d2028ecb4c074306c8  fonts/jetbrains-mono-vietnamese-400-normal.woff2
3911b66d9f2e005a4b989223405d0e5032619c668597ba467cc76a23c8fffcfb  fonts/manrope-latin-ext-wght-normal.woff2
a30ddcd349703aff7464c34bef3fffdff405ee50c113440d7c8693c02d210972  fonts/manrope-latin-wght-normal.woff2
6bbb044ab420e07edb0a3042d2eb314b85e83a0182e945a15ab3b9092668dfd5  fonts/manrope-vietnamese-wght-normal.woff2
403581b69dac5cff4079205e01c6b467e56af449ecbd7247693ddb1baafa005b  LICENSES/jetbrains-LICENSE.txt
b740a1d46122672da62833e97f7e7c8a13fa85cbc7445b584b297cc00dde93db  LICENSES/LICENSE-Tabler.txt
d826ab6583b12c26807d8716a545bdbbb672df04f48608a364ba9efdbe501c30  LICENSES/manrope-LICENSE.txt
```

The TinyOrbit name, wordmark and mark are trademarks — see `TRADEMARKS.md`
at the repository root; they are not covered by the MIT licence.
