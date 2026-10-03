import { describe, expect, it } from "vitest";
import {
  LAG_MAX,
  STALL_MS,
  createTracker,
  deriveProgress,
  formatElapsed,
  noteServerDateHeader,
  opPollMs,
  serverNow,
  startInFlight,
  withJitter,
  workspacePollMs,
  type WorkspaceView,
} from "../../../src/progress/derive";
import { makeWorkspace, type ConditionFixture } from "../../mock-api/fixtures.ts";

const T0 = "2026-10-03T10:00:00Z";
const NOW = Date.parse(T0);

function cond(type: ConditionFixture["type"], status: ConditionFixture["status"], reason: string): ConditionFixture {
  return { type, status, reason, lastTransitionTime: "2026-10-03T09:59:00Z" };
}

function ws(overrides: Partial<WorkspaceView> = {}): WorkspaceView {
  return makeWorkspace({
    phase: "Provisioning",
    desiredState: "Running",
    conditions: [cond("Admitted", "True", "QuotaReserved")],
    createdAt: "2026-10-03T09:50:00Z",
    updatedAt: T0,
    ...overrides,
  });
}

const states = (m: ReturnType<typeof deriveProgress>) => m!.steps.map((s) => s.state);
const active = (m: ReturnType<typeof deriveProgress>) => m!.steps[m!.current];

describe("deriveProgress — op detection", () => {
  it("idle states derive nothing", () => {
    expect(
      deriveProgress(
        ws({ phase: "Ready", conditions: [cond("ConnectionReady", "True", "Ready")] }),
        undefined,
        NOW,
      ),
    ).toBeNull();
    expect(
      deriveProgress(ws({ phase: "Stopped", desiredState: "Stopped" }), undefined, NOW),
    ).toBeNull();
    expect(
      deriveProgress(
        ws({
          phase: "Ready",
          desiredState: "Running",
          conditions: [cond("ConnectionReady", "True", "Ready")],
        }),
        undefined,
        NOW,
      ),
    ).toBeNull();
  });

  it("the Ready→ConnectionReady gap stays in-flight on the connect step (R-V3b M1)", () => {
    // phase Ready but the stream endpoint has not registered yet.
    const gap = ws({
      phase: "Ready",
      conditions: [
        cond("Admitted", "True", "QuotaReserved"),
        cond("StorageReady", "True", "VolumeBound"),
        cond("RuntimeReady", "True", "Ready"),
        cond("ConnectionReady", "False", "StreamDown"),
      ],
    });
    const m = deriveProgress(gap, undefined, NOW);
    expect(m?.op).toBe("start");
    expect(active(m).id).toBe("connect");
    expect(active(m).reason).toBe("StreamDown");
    expect(startInFlight(gap)).toBe(true);
    // A missing condition is the same gap (endpoint never published).
    const missing = ws({ phase: "Ready", conditions: [] });
    expect(deriveProgress(missing, undefined, NOW)).not.toBeNull();
    expect(startInFlight(missing)).toBe(true);
  });

  it("create: Pending with desiredState Running and createdAt==updatedAt", () => {
    const m = deriveProgress(
      ws({ phase: "Pending", createdAt: T0, updatedAt: T0 }),
      undefined,
      NOW,
    );
    expect(m?.op).toBe("create");
    expect(m?.title).toBe("progress.create.title");
    expect(states(m)).toEqual(["done", "active", "waiting", "waiting", "waiting"]);
    expect(active(m).id).toBe("disk");
  });

  it("start: Stopped + desired Running is the queued state, not create", () => {
    const m = deriveProgress(
      ws({ phase: "Stopped", desiredState: "Running" }),
      undefined,
      NOW,
    );
    expect(m?.op).toBe("start");
    expect(states(m)).toEqual(["active", "waiting", "waiting", "waiting", "waiting"]);
    expect(active(m).label).toBe("progress.step.scheduling");
  });

  it("stop: FX-R19 — Terminating wins over desiredState Stopped", () => {
    const m = deriveProgress(
      ws({ phase: "Terminating", desiredState: "Stopped" }),
      undefined,
      NOW,
    );
    expect(m?.op).toBe("delete");
    const stop = deriveProgress(
      ws({ phase: "Stopping", desiredState: "Stopped" }),
      undefined,
      NOW,
    );
    expect(stop?.op).toBe("stop");
    expect(states(stop)).toEqual(["done", "active", "waiting"]);
  });

  it("stop queued: Ready + desired Stopped keeps step 1 active", () => {
    const m = deriveProgress(ws({ phase: "Ready", desiredState: "Stopped" }), undefined, NOW);
    expect(m?.op).toBe("stop");
    expect(active(m).label).toBe("progress.step.queued");
  });
});

describe("deriveProgress — start steps and reason tokens", () => {
  it("disk step is hidden for Ephemeral", () => {
    const m = deriveProgress(ws({ dataPolicy: "Ephemeral" }), undefined, NOW);
    expect(m?.steps.map((s) => s.id)).toEqual(["accepted", "machine", "desktop", "connect"]);
    expect(active(m).id).toBe("machine");
  });

  it("disk active while StorageReady is False/Provisioning", () => {
    const m = deriveProgress(
      ws({ conditions: [cond("StorageReady", "False", "Provisioning")] }),
      undefined,
      NOW,
    );
    expect(active(m).id).toBe("disk");
    expect(active(m).reason).toBe("Provisioning");
  });

  it("attach: the disk step is labelled for the retained claim", () => {
    const m = deriveProgress(
      ws({
        retainedDataRef: "rd_01J4Z9W2PFK8G4TQ3M7H1R5N0A",
        conditions: [cond("StorageReady", "False", "WaitingForDisk")],
      }),
      undefined,
      NOW,
    );
    expect(active(m).id).toBe("disk");
    expect(active(m).label).toBe("progress.step.diskAttach");
    expect(active(m).reason).toBe("WaitingForDisk");
  });

  it("machine step: Provisioning and Unschedulable keep it active", () => {
    for (const reason of ["Provisioning", "Unschedulable"]) {
      const m = deriveProgress(
        ws({
          conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", reason)],
        }),
        undefined,
        NOW,
      );
      expect(active(m).id).toBe("machine");
      expect(active(m).reason).toBe(reason);
    }
    const unsched = deriveProgress(
      ws({
        conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", "Unschedulable")],
      }),
      undefined,
      NOW,
    );
    expect(active(unsched).notice).toBe("progress.slow.machine");
  });

  it("desktop step label follows the pod reason (G2 tokens)", () => {
    const cases: [string, string][] = [
      ["PreparingPod", "progress.step.machinePrepare"],
      ["PullingImage", "progress.step.imagePull"],
      ["ContainerCreating", "progress.step.desktop"],
      ["PodInitializing", "progress.step.desktop"],
      ["NotReady", "progress.step.desktop"],
      ["CrashLoopBackOff", "progress.step.desktopStart"],
    ];
    for (const [reason, label] of cases) {
      const m = deriveProgress(
        ws({
          conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", reason)],
        }),
        undefined,
        NOW,
      );
      expect(active(m).id, reason).toBe("desktop");
      expect(active(m).label, reason).toBe(label);
    }
  });

  it("image pull failures carry their copy while still retrying", () => {
    const m = deriveProgress(
      ws({
        conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", "ImagePullBackOff")],
      }),
      undefined,
      NOW,
    );
    expect(active(m).state).toBe("active");
    expect(active(m).notice).toBe("progress.failed.imagePull");
  });

  it("BackendError surfaces on the machine step with the retry note", () => {
    const m = deriveProgress(
      ws({
        conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", "BackendError")],
      }),
      undefined,
      NOW,
    );
    expect(active(m).id).toBe("machine");
    expect(active(m).notice).toBe("progress.notice.retry");
  });

  it("connect step waits for ConnectionReady while runtime is up", () => {
    const m = deriveProgress(
      ws({
        conditions: [
          cond("StorageReady", "True", "Ready"),
          cond("RuntimeReady", "True", "Ready"),
          cond("ConnectionReady", "False", "Provisioning"),
        ],
      }),
      undefined,
      NOW,
    );
    expect(active(m).id).toBe("connect");
    expect(states(m)).toEqual(["done", "done", "done", "done", "active"]);
  });

  it("an unknown reason token is kept verbatim, never hidden", () => {
    const m = deriveProgress(
      ws({
        conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", "BrandNewToken")],
      }),
      undefined,
      NOW,
    );
    expect(active(m).reason).toBe("BrandNewToken");
    expect(active(m).unknownReason).toBe(true);
  });
});

describe("deriveProgress — delete steps (G5 teardown marks)", () => {
  const del = (conds: ConditionFixture[], over: Partial<WorkspaceView> = {}) =>
    deriveProgress(
      ws({ phase: "Terminating", desiredState: "Stopped", conditions: conds, ...over }),
      undefined,
      NOW,
    );

  it("marks sessions active during BlockingConnects..DrainingStreams", () => {
    for (const reason of ["BlockingConnects", "RevokingLeases", "DrainingStreams"]) {
      const m = del([cond("RuntimeReady", "False", reason)]);
      expect(active(m).id, reason).toBe("sessions");
      expect(states(m)).toEqual(["done", "active", "waiting", "waiting", "waiting"]);
    }
  });

  it("Degraded StreamDraining shows the drain note", () => {
    const m = del([cond("Degraded", "True", "StreamDraining")]);
    expect(active(m).id).toBe("sessions");
    expect(active(m).notice).toBe("progress.slow.drain");
  });

  it("StoppingRuntime advances to removing the desktop", () => {
    const m = del([cond("RuntimeReady", "False", "StoppingRuntime")]);
    expect(active(m).id).toBe("runtime");
  });

  it("ApplyingRetention reaches the data step for Retain only", () => {
    const retain = del([cond("RuntimeReady", "False", "ApplyingRetention")]);
    expect(active(retain).id).toBe("data");
    const eph = del([cond("RuntimeReady", "False", "ApplyingRetention")], { dataPolicy: "Ephemeral" });
    expect(eph?.steps.map((s) => s.id)).not.toContain("data");
    expect(active(eph).id).toBe("finishing");
  });

  it("CleaningUp and an empty condition set land on finishing", () => {
    expect(active(del([cond("RuntimeReady", "False", "CleaningUp")])).id).toBe("finishing");
    expect(active(del([])).id).toBe("finishing");
  });

  it("a CleanupRetry degraded reason shows the retry note on the active step", () => {
    const m = del([
      cond("RuntimeReady", "False", "DrainingStreams"),
      cond("Degraded", "True", "CleanupRetry"),
    ]);
    expect(active(m).reason).toBe("CleanupRetry");
    expect(active(m).notice).toBe("progress.failed.cleanup");
  });
});

describe("deriveProgress — failed terminal", () => {
  it("locates the stalled step from ConnectionReady's kept pod reason", () => {
    const m = deriveProgress(
      ws({
        phase: "Failed",
        desiredState: "Running",
        failureReason: "BootDeadlineExceeded",
        conditions: [
          cond("Admitted", "True", "QuotaReserved"),
          cond("StorageReady", "True", "Ready"),
          cond("RuntimeReady", "False", "BootDeadlineExceeded"),
          cond("ConnectionReady", "False", "PullingImage"),
        ],
      }),
      undefined,
      NOW,
    );
    expect(m?.terminal).toBe("failed");
    const failed = m!.steps.find((s) => s.state === "failed");
    expect(failed?.id).toBe("desktop");
    expect(failed?.label).toBe("progress.step.imagePull");
    expect(failed?.notice).toBe("progress.failed.deadline");
    expect(m!.steps.at(-1)?.state).toBe("skipped");
  });

  it("image-pull failure keeps its reason copy at Failed", () => {
    const m = deriveProgress(
      ws({
        phase: "Failed",
        desiredState: "Running",
        failureReason: "BootDeadlineExceeded",
        conditions: [cond("ConnectionReady", "False", "ErrImagePull")],
      }),
      undefined,
      NOW,
    );
    const failed = m!.steps.find((s) => s.state === "failed");
    expect(failed?.id).toBe("desktop");
    expect(failed?.notice).toBe("progress.failed.imagePull");
  });
});

describe("progress tracker — replica-lag latch", () => {
  const atMachine = () =>
    ws({ conditions: [cond("StorageReady", "True", "Ready"), cond("RuntimeReady", "False", "Provisioning")] });
  const atDisk = () => ws({ conditions: [] });

  it("ignores a poll that shows an earlier step for up to LAG_MAX polls", () => {
    const tr = createTracker();
    expect(deriveProgress(atMachine(), tr, NOW)?.current).toBe(2);
    for (let i = 0; i < LAG_MAX; i++) {
      const m = deriveProgress(atDisk(), tr, NOW + (i + 1) * 1000);
      expect(m?.current, `lag ${i + 1}`).toBe(2);
    }
    const accepted = deriveProgress(atDisk(), tr, NOW + (LAG_MAX + 1) * 1000);
    expect(accepted?.current).toBe(1);
  });

  it("a phase change is authoritative — resets the latch immediately", () => {
    const tr = createTracker();
    deriveProgress(atMachine(), tr, NOW);
    const regressed = ws({
      phase: "Stopping",
      desiredState: "Stopped",
      conditions: [cond("Admitted", "True", "QuotaReserved")],
    });
    const m = deriveProgress(regressed, tr, NOW + 1000);
    expect(m?.op).toBe("stop");
    expect(m?.current).toBe(1); // shutdown — not latched to the old index
  });

  it("a new intent (updatedAt) resets the tracker", () => {
    const tr = createTracker();
    deriveProgress(atMachine(), tr, NOW);
    const m = deriveProgress(atDisk(), { ...tr }, NOW + 1000);
    // sanity: same key still latches
    expect(m?.current).toBe(2);
    const newer = { ...atDisk(), updatedAt: "2026-10-03T10:05:00Z" };
    expect(deriveProgress(newer, tr, NOW + 2000)?.current).toBe(1);
  });

  it("StatusStale marks the view delayed and never advances the latch", () => {
    const tr = createTracker();
    expect(deriveProgress(atDisk(), tr, NOW)?.current).toBe(1);
    const stale = {
      ...atMachine(),
      conditions: [
        ...atMachine().conditions,
        cond("Degraded", "True", "StatusStale"),
      ],
    };
    const m = deriveProgress(stale, tr, NOW + 1000);
    expect(m?.delayed).toBe(true);
    expect(m?.current).toBe(1);
  });

  it("repeating the same observation never counts as lag", () => {
    const tr = createTracker();
    deriveProgress(atMachine(), tr, NOW);
    const lag = atDisk();
    deriveProgress(lag, tr, NOW + 1000);
    deriveProgress(lag, tr, NOW + 2000);
    deriveProgress(lag, tr, NOW + 3000);
    // Same signature thrice → lag counted once → still clamped.
    expect(deriveProgress(lag, tr, NOW + 4000)?.current).toBe(2);
  });
});

describe("polling cadence and elapsed", () => {
  it("opPollMs follows the cadence table", () => {
    expect(opPollMs(NOW, NOW)).toBe(1_500);
    expect(opPollMs(NOW, NOW + 19_999)).toBe(1_500);
    expect(opPollMs(NOW, NOW + 20_000)).toBe(3_000);
    expect(opPollMs(NOW, NOW + 119_999)).toBe(3_000);
    expect(opPollMs(NOW, NOW + 120_000)).toBe(5_000);
    expect(opPollMs(NOW, NOW + 600_000)).toBe(10_000);
    expect(opPollMs(NOW + 60_000, NOW)).toBe(1_500); // future anchor clamps at 0
  });

  it("withJitter stays within ±20 %", () => {
    expect(withJitter(10_000, 0)).toBe(8_000);
    expect(withJitter(10_000, 0.999)).toBe(11_996);
    expect(withJitter(10_000, 0.5)).toBe(10_000);
  });

  it("workspacePollMs picks the op cadence and falls back to idle", () => {
    expect(workspacePollMs(ws({}), 8_000, NOW)).toBe(1_500);
    expect(
      workspacePollMs(
        ws({ phase: "Ready", conditions: [cond("ConnectionReady", "True", "Ready")] }),
        8_000,
        NOW,
      ),
    ).toBe(8_000);
    expect(
      workspacePollMs(ws({ phase: "Failed", failureReason: "BootDeadlineExceeded" }), 8_000, NOW),
    ).toBe(8_000); // terminal is not busy
    expect(workspacePollMs(undefined, 8_000, NOW)).toBe(8_000);
  });

  it("elapsed clamps at 0 and stalls past the threshold", () => {
    const m = deriveProgress(ws({ updatedAt: "2026-10-03T10:01:00Z" }), undefined, NOW);
    expect(m?.elapsedMs).toBe(0);
    const stalled = deriveProgress(
      ws({ updatedAt: "2026-10-03T09:40:00Z" }),
      undefined,
      NOW,
    );
    expect(stalled?.stalled).toBe(true);
    expect(NOW - Date.parse("2026-10-03T09:40:00Z")).toBeGreaterThan(STALL_MS);
  });

  it("startInFlight is true only for live start/create", () => {
    expect(startInFlight(ws({}))).toBe(true);
    expect(startInFlight(ws({ phase: "Failed" }))).toBe(false);
    expect(startInFlight(ws({ phase: "Terminating" }))).toBe(false);
    expect(
      startInFlight(
        ws({ phase: "Ready", conditions: [cond("ConnectionReady", "True", "Ready")] }),
      ),
    ).toBe(false);
    expect(startInFlight(ws({ phase: "Stopped", desiredState: "Stopped" }))).toBe(false);
  });

  it("formatElapsed renders m:ss and h:mm:ss", () => {
    expect(formatElapsed(35_000)).toBe("0:35");
    expect(formatElapsed(9 * 60_000 + 5_000)).toBe("9:05");
    expect(formatElapsed(3_723_000)).toBe("1:02:03");
  });
});

describe("server clock skew", () => {
  // Runs last: the skew is module state, shared by every serverNow().
  it("a Date header corrects the local clock", () => {
    const ahead = Date.now() + 2 * 60_000;
    noteServerDateHeader(new Date(ahead).toUTCString());
    const skew = serverNow() - Date.now();
    expect(skew).toBeGreaterThan(100_000);
    expect(skew).toBeLessThan(140_000);
    noteServerDateHeader(null); // no header: ignored
    noteServerDateHeader("not a date");
    const skew2 = serverNow() - Date.now();
    expect(Math.abs(skew2 - skew)).toBeLessThanOrEqual(1);
  });
});
