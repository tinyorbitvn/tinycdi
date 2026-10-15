// Tests for drills.sh against a fake kubectl that records its arguments.
// No cluster involved. Run: npm test.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import assert from "node:assert/strict";

const DRILLS = path.join(import.meta.dirname, "drills.sh");

function fakeKubectl(): { bin: string; calls: () => string[] } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-drills-"));
  const log = path.join(dir, "calls.log");
  const bin = path.join(dir, "kubectl");
  fs.writeFileSync(
    bin,
    `#!/usr/bin/env bash
echo "$*" >> "${log}"
case "$*" in
  *"get pod"*) echo backend-abc ;;
  *"get certificate"*)
    if [ -n "\${FAKE_FAIL_GET_CERT:-}" ]; then echo "error: the server doesn't have a resource type certificate" >&2; exit 1; fi
    printf 'tls-secret\\tmy-cert\\n' ;;
esac
exit 0
`,
    { mode: 0o755 },
  );
  return { bin, calls: () => (fs.existsSync(log) ? fs.readFileSync(log, "utf8").trim().split("\n") : []) };
}

function drills(args: string[], env: Record<string, string> = {}) {
  const k = fakeKubectl();
  const r = spawnSync("bash", [DRILLS, ...args], {
    encoding: "utf8",
    env: { ...process.env, KUBECTL: k.bin, PATH: "/usr/bin:/bin", ...env },
  });
  return { ...r, calls: k.calls() };
}

test("R5c: options are accepted before the drill name", () => {
  const r = drills(["-n", "prod", "--deployment", "web", "rollout"]);
  assert.equal(r.status, 0, r.stderr);
  assert.ok(r.calls.includes("-n prod rollout restart deployment/web"), r.calls.join("\n"));
});

test("R5c: options are accepted after the drill name (README form)", () => {
  const r = drills(["rollout", "-n", "prod", "--deployment", "web"]);
  assert.equal(r.status, 0, r.stderr);
  assert.ok(r.calls.includes("-n prod rollout restart deployment/web"), r.calls.join("\n"));
});

test("R5c: rotate-cert --certificate after the drill name reaches the renewal", () => {
  const r = drills(["rotate-cert", "--certificate", "tinycdi-session-tls"]);
  // No cmctl in PATH and the fake kubectl answers `cert-manager version` ok.
  assert.equal(r.status, 0, r.stderr);
  assert.ok(
    r.calls.some((c) => /cert-manager renew tinycdi-session-tls/.test(c)),
    r.calls.join("\n"),
  );
});

test("R5c: an unknown option is an error, before or after the drill name", () => {
  for (const argv of [["--bogus", "rollout"], ["rollout", "--bogus"], ["rollout", "--bogus", "x"]]) {
    const r = drills(argv);
    assert.notEqual(r.status, 0, argv.join(" "));
    assert.match(r.stderr, /unknown (flag|option).*--bogus/, argv.join(" "));
    assert.deepEqual(r.calls, [], "nothing may run on a usage error");
  }
});

test("R5c: a second drill name is an error", () => {
  const r = drills(["rollout", "delete-pod"]);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /unexpected argument.*delete-pod/);
  assert.deepEqual(r.calls, []);
});

test("R5c: an option missing its value is an error", () => {
  const r = drills(["rollout", "--deployment"]);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /--deployment needs a value/);
});

test("R5e: rotate-cert has no --touch mode (an annotation rewrites no file)", () => {
  const r = drills(["rotate-cert", "--secret", "tls-secret", "--touch"]);
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /unknown (flag|option).*--touch/);
  assert.ok(!r.calls.some((c) => c.includes("annotate")), "must never annotate the Secret");
});

test("R5e: rotate-cert --secret re-issues through the owning Certificate", () => {
  const r = drills(["rotate-cert", "--secret", "tls-secret"]);
  assert.equal(r.status, 0, r.stderr);
  assert.ok(r.calls.some((c) => /cert-manager renew my-cert/.test(c)), r.calls.join("\n"));
  assert.ok(!r.calls.some((c) => c.includes("annotate")));
});

test("R5e: rotate-cert --secret with no owning Certificate fails instead of faking a rotation", () => {
  const r = drills(["rotate-cert", "--secret", "other-secret"]);
  assert.notEqual(r.status, 0);
  assert.match(r.stdout + r.stderr, /no cert-manager Certificate/);
  assert.ok(!r.calls.some((c) => c.includes("annotate")));
});

test("R5e: delete-pod waits with rollout status on the Deployment, not kubectl wait", () => {
  const r = drills(["delete-pod"]);
  assert.equal(r.status, 0, r.stderr);
  assert.ok(r.calls.some((c) => /delete pod backend-abc/.test(c)), r.calls.join("\n"));
  assert.ok(
    r.calls.some((c) => /rollout status deployment\/backend/.test(c)),
    r.calls.join("\n"),
  );
  assert.ok(!r.calls.some((c) => /(^| )wait( |$)/.test(c)), "kubectl wait on pods is racy");
});

test("R5f: a failing `kubectl get certificate` is reported, not a silent exit", () => {
  const r = drills(["rotate-cert", "--secret", "tls-secret"], { FAKE_FAIL_GET_CERT: "1" });
  assert.notEqual(r.status, 0);
  assert.match(r.stdout + r.stderr, /could not list certificates/);
  assert.match(r.stdout + r.stderr, /doesn't have a resource type/, "kubectl's own message is surfaced");
});

test("R5f: usage prints the header, not script internals", () => {
  const r = drills(["--help"]);
  assert.equal(r.status, 0);
  assert.match(r.stdout, /drills\.sh \[options\] delete-pod/);
  assert.doesNotMatch(r.stdout, /set -euo pipefail/);
  assert.doesNotMatch(r.stdout, /^KUBECTL=/m);
});
