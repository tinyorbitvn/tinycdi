# Node security profiles — TinyCDI browser workspaces

The browser `WorkspaceTemplate` confines its pods with a **Localhost seccomp
profile** plus a **Localhost AppArmor profile** so Chromium's user-namespace
sandbox works inside an otherwise restricted pod (`runAsNonRoot`,
`allowPrivilegeEscalation: false`, `drop ALL`). Both profiles must be
**pre-loaded on every node that can run browser sessions** — Kubernetes does
not distribute them; installation is GitOps/node-provisioning owned.

| File | Purpose |
|---|---|
| `seccomp/chromium-userns.json` | Resolved OCI `LinuxSeccomp` profile (plain `architectures` + unconditional `syscalls`; amd64, zero-capability context). sha256 `c57199218f6ba2e6616f39392f3043b3e057fdfa082ee90f0798170c5715d76a` — locked in `docs/compatibility.md`. |
| `seccomp/chromium-userns.src.json` | Docker-dialect source of truth (moby `profiles/seccomp/default.json` @ v27.5.1 + appended userns group). **Do not deploy directly** — its `includes`/`excludes`/`archMap` are silently ignored by containerd, which would broaden the profile. |
| `seccomp/resolve-seccomp.py` | Deterministic resolver: evaluates every conditional group for the pod context (amd64, drop ALL caps, kernel ≥6.8) and emits the OCI artifact. Regenerate/verify with `python3 resolve-seccomp.py chromium-userns.src.json out.json` — output is byte-identical to `chromium-userns.json`. |
| `apparmor/tinycdi-browser` | `cri-containerd.apparmor.d` base template + a single added `userns,` rule, under profile name `tinycdi-browser`. Required on nodes with `kernel.apparmor_restrict_unprivileged_userns=1` (e.g. Ubuntu 24.04) where CLONE_NEWUSER is mediated. |

## Installing on a browser-capable node

Do this on every node before browser workspaces are scheduled there (the
chart places them by `nodeSelector`). Two options:

- **Chart-managed (recommended for GitOps):** set
  `nodeProfiles.install.enabled=true` on `deploy/helm/tinycdi`. The chart
  ships byte-identical copies of `seccomp/chromium-userns.json` and
  `apparmor/tinycdi-browser` (under `files/node-profiles/`, kept in sync by
  a chart test) and runs a hostPID `node-profiles` DaemonSet in a
  DEDICATED privileged-PSS namespace (`nodeProfiles.install.namespace`,
  never the release namespace) that performs exactly the steps below —
  atomic seccomp write, daemon-peer placeholder resolution, host
  `apparmor_parser` load via `nsenter -t 1 -m` — in a one-shot privileged
  initContainer; the long-running container is a uid-0 verifier with every
  capability dropped, because only root can read the kernel's
  loaded-profiles list (CHTR-1). See the chart README "Node profiles" for
  the rollout race, readiness gating and `mode: remove`.
- **Manual / configuration management:** the steps below.

### Seccomp

```sh
install -m 0644 seccomp/chromium-userns.json \
  /var/lib/kubelet/seccomp/profiles/chromium-userns.json
# <kubelet-root> defaults to /var/lib/kubelet; adjust for your distribution.
```

The kubelet reads Localhost profiles at container-creation time — no kubelet
restart is needed; new pods pick the file up immediately.

### AppArmor

1. **Substitute the daemon peer placeholder first.** The upstream template's
   `signal (receive) peer=cri-containerd` line must carry the AppArmor profile
   name of the node's actual containerd process — read it on the node:

   ```sh
   cat /sys/kernel/security/apparmor/profiles   # e.g. cri-containerd.apparmor.d
   ```

   then `sed -i 's/peer=cri-containerd/peer=<daemon-profile>/' tinycdi-browser`
   if it differs.

2. Compile-check, then load:

   ```sh
   apparmor_parser -Q -K tinycdi-browser   # compile check only
   apparmor_parser -r -K tinycdi-browser   # load / replace (-r)
   ```

   Use `-K` (skip cache read) and never `-W` — the cache on a managed node can
   serve a stale compiled profile. To reload an updated profile, `-r -K`
   again; to add without replacing, `-a -K`.

## Verifying

```sh
# Seccomp file present and referenced correctly:
ls -l /var/lib/kubelet/seccomp/profiles/chromium-userns.json

# AppArmor profile loaded in enforce mode:
grep tinycdi-browser /sys/kernel/security/apparmor/profiles

# Inside a running browser pod — AppArmor confinement:
kubectl -n <ns> exec <browser-pod> -- cat /proc/1/attr/current
#   -> tinycdi-browser (enforce)

# Seccomp + nested userns engaged on a renderer process
# (Chromium child inside its own user namespace):
kubectl -n <ns> exec <browser-pod> -- grep -E '^(Seccomp|Seccomp_filters|NStgid)' \
  /proc/<renderer-pid>/status
#   -> Seccomp: 2 (filter mode), Seccomp_filters: 2
#   (container Localhost profile + Chromium's own seccomp-bpf layer)
kubectl -n <ns> exec <browser-pod> -- cat /proc/<renderer-pid>/uid_map
#   -> shows the nested user-namespace mapping
```

If `/proc/1/attr/current` prints `unconfined` or a renderer lacks the second
filter, the profile was not bound — check the pod's `workspaces.cdi.tinyorbit.vn/
seccomp-profile` / `workspaces.cdi.tinyorbit.vn/apparmor-profile` annotations and
the node's loaded profiles.

## Rolling back

Profiles are consumed **at container creation** — removing them does not
disturb running pods, but a pod referencing a missing Localhost profile fails
to start. Order matters:

1. Drain browser workloads off the node (or drop the template's
   `seccompProfile`/`appArmorProfile` keys — an unset annotation falls back
   to RuntimeDefault/containerd-default confinement. Do NOT write
   `unconfined`: the chart renders it as `localhost/unconfined`, which
   resolves to the kernel's unconfined pseudo-profile and strips AppArmor
   confinement entirely).
2. Remove the seccomp file:
   `rm /var/lib/kubelet/seccomp/profiles/chromium-userns.json`
3. Unload the AppArmor profile:
   `apparmor_parser -R -K tinycdi-browser`
4. Reschedule; verify with the checks above (expect the containerd default
   profile — e.g. `cri-containerd.apparmor.d (enforce)` — and
   `Seccomp_filters: 1`).

## Chart references

Browser `WorkspaceTemplate`s in `deploy/helm/tinycdi` select these profiles
per template:

```yaml
templates:
  - name: browser
    seccompProfile: localhost/profiles/chromium-userns.json
    appArmorProfile: tinycdi-browser
```

which the chart turns into pod annotations
`workspaces.cdi.tinyorbit.vn/seccomp-profile: localhost/profiles/chromium-userns.json`
and `workspaces.cdi.tinyorbit.vn/apparmor-profile: localhost/tinycdi-browser`.
