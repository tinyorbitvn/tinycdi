#!/usr/bin/env bash
# publish-gating.test.sh — R1-38 / TM-49: per-job (YAML-parsed) guards on
# the signing chain. In each publishing workflow exactly one job holds
# `id-token: write`; it needs the scan job (and, under always(), requires
# its success); the release and train signers sit behind their
# environments; the main-channel and train signers run only on
# refs/heads/main; no workflow grants id-token at the top level.
#
# Usage: publish-gating.test.sh [<repo-root>] — the optional root lets a
# reviewer run the guards against a mutated copy of the workflows.
set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
fails=0

python3 -I - "$ROOT" <<'PY' || fails=1
import sys
try:
    import yaml
except ImportError:
    print("FAIL: PyYAML is not importable — the signing-chain guards cannot run")
    sys.exit(1)
root = sys.argv[1]
bad = 0
def fail(msg):
    global bad
    print("FAIL:", msg)
    bad = 1
spec = {
    "images.yml":         {"signer": "promote", "env": None,            "main_only": True},
    "release.yml":        {"signer": "publish", "env": "release",       "main_only": False},
    "runtime-images.yml": {"signer": "publish", "env": "runtime-train", "main_only": True},
}
for wf, s in spec.items():
    with open(f"{root}/.github/workflows/{wf}") as fh:
        d = yaml.safe_load(fh)
    jobs = d["jobs"]
    if (d.get("permissions") or {}).get("id-token") == "write":
        fail(f"{wf}: workflow-level id-token: write")
    minting = sorted(j for j, b in jobs.items()
                     if (b.get("permissions") or {}).get("id-token") == "write")
    if minting != [s["signer"]]:
        fail(f"{wf}: id-token: write granted to {minting}, want only [{s['signer']}]")
    sj = jobs.get(s["signer"], {})
    needs = sj.get("needs") or []
    needs = [needs] if isinstance(needs, str) else needs
    if "scan" not in needs:
        fail(f"{wf}: {s['signer']} does not need the scan job (needs={needs})")
    cond = str(sj.get("if") or "")
    if "always()" in cond and "needs.scan.result == 'success'" not in cond:
        fail(f"{wf}: {s['signer']} runs under always() without requiring scan success")
    if s["env"] and sj.get("environment") != s["env"]:
        fail(f"{wf}: {s['signer']} environment={sj.get('environment')!r}, want {s['env']!r}")
    if s["main_only"] and "github.ref == 'refs/heads/main'" not in cond:
        fail(f"{wf}: {s['signer']} is not restricted to refs/heads/main (if={cond!r})")
sys.exit(bad)
PY

if [ "$fails" -eq 0 ]; then echo "publish-gating: signing chain pinned per job"; fi
exit "$fails"
