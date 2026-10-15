#!/usr/bin/env python3
"""Resolve the Docker-dialect seccomp base profile into a concrete OCI
LinuxSeccomp profile for the tinycdi workspace pod context.

Docker/moby profiles carry archMap + conditional includes/excludes (caps,
arches, minKernel). Kubernetes Localhost / containerd expect a resolved OCI
profile: `architectures` + unconditional `syscalls` rules. If conditional keys
were silently dropped instead, cap-gated groups (e.g. the CAP_SYS_ADMIN group
containing mount/bpf/perf_event_open) would apply unconditionally — a silent
broadening. This resolver evaluates every condition for a fixed context and
emits only what applies.

Context (must match the pod spec):
  - arch: amd64 (SCMP_ARCH_X86_64)
  - container caps: none (drop ALL + allowPrivilegeEscalation:false)
  - node kernel: 6.8 (>= all minKernel gates in the pinned base)

Usage: python3 resolve-seccomp.py base.json out.json
"""
import json, sys

ARCH = "amd64"          # docker-dialect arch name in the base profile
ARCH_OCI = "SCMP_ARCH_X86_64"
CAPS = set()            # pod drops ALL capabilities
KERNEL = (6, 8)

def applies(cond):
    inc, exc = cond.get("includes") or {}, cond.get("excludes") or {}
    if any(c not in CAPS for c in inc.get("caps", [])):
        return False
    if "arches" in inc and ARCH not in inc["arches"]:
        return False
    if "minKernel" in inc and KERNEL < tuple(map(int, inc["minKernel"].split("."))):
        return False
    if any(c in CAPS for c in exc.get("caps", [])):
        return False
    if "arches" in exc and ARCH in exc["arches"]:
        return False
    return True

def resolve(base_path, out_path):
    d = json.load(open(base_path))
    if "archMap" not in d:
        sys.exit("base is not Docker-dialect (no archMap) — refuse to resolve")
    out = {
        "defaultAction": d["defaultAction"],
        "architectures": [ARCH_OCI],
    }
    if d.get("defaultErrnoRet") is not None:
        out["defaultErrnoRet"] = d["defaultErrnoRet"]
    rules = []
    for s in d["syscalls"]:
        if not applies(s):
            continue
        r = {"names": s["names"], "action": s["action"]}
        if s.get("args"):
            r["args"] = s["args"]
        if s.get("errnoRet") is not None:
            r["errnoRet"] = s["errnoRet"]
        if s.get("comment"):
            r["comment"] = s["comment"]
        rules.append(r)
    out["syscalls"] = rules
    dropped = [s["names"][:3] for s in d["syscalls"] if not applies(s)]
    print(f"applied {len(rules)}/{len(d['syscalls'])} groups; skipped (cap/arch/kernel-gated): {dropped}")
    json.dump(out, open(out_path, "w"), indent=2)
    print(f"wrote {out_path}")

if __name__ == "__main__":
    resolve(sys.argv[1], sys.argv[2])
