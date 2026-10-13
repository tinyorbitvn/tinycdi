import { describe, expect, it } from "vitest";
import {
  COLD_BACKOFF_MS,
  COLD_BUDGET_MS,
  COLD_CONFIRM_MS,
  nextColdStep,
  startCold,
  streamConfirmed,
} from "../../../src/session/coldConnect";

describe("streamConfirmed", () => {
  it("needs an open stream on a live lease", () => {
    expect(streamConfirmed({ state: "connected", leaseActive: true })).toBe(true);
    expect(streamConfirmed({ state: "disconnected", leaseActive: true })).toBe(false);
    expect(streamConfirmed({ state: "stale", leaseActive: true })).toBe(false);
    expect(streamConfirmed({ state: "none", leaseActive: false })).toBe(false);
    expect(streamConfirmed({ state: "connected", leaseActive: false })).toBe(false);
  });
});

describe("nextColdStep", () => {
  it("backs off 1 s, 2 s, 4 s, then keeps 4 s", () => {
    const a = startCold(0);
    const delays: number[] = [];
    let now = COLD_CONFIRM_MS;
    for (let i = 0; i < 4; i++) {
      const step = nextColdStep(a, now);
      if (step.kind !== "relaunch") throw new Error(`gave up at ${i}`);
      delays.push(step.delayMs);
      a.relaunches++;
      now += step.delayMs + COLD_CONFIRM_MS;
    }
    expect(delays).toEqual([...COLD_BACKOFF_MS, COLD_BACKOFF_MS[COLD_BACKOFF_MS.length - 1]]);
  });

  it("gives up when another confirm window would end past the budget", () => {
    const a = startCold(0);
    expect(nextColdStep(a, COLD_BUDGET_MS - COLD_CONFIRM_MS - 500).kind).toBe("give-up");
    expect(nextColdStep(startCold(0), COLD_CONFIRM_MS).kind).toBe("relaunch");
  });
});
