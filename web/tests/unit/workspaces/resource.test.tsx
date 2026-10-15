// FX-R29 — useResource.refresh() re-arms a stopped poll: a consumer whose
// interval function returns null once data is settled (the retained-data
// list during a purge) must resume polling when a manual refresh
// discovers transitional data. An already-armed poll is untouched and a
// hidden tab never gets a timer.
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useResource } from "../../../src/workspaces/resource";

const flush = () => act(async () => {});
const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useResource refresh re-arm (FX-R29)", () => {
  it("resumes a stopped poll when refresh discovers transitional data", async () => {
    let current = "idle";
    const load = vi.fn(() => Promise.resolve(current));
    const { result } = renderHook(() =>
      useResource(load, (d) => (d === "busy" ? 1_000 : null)),
    );
    await flush();
    expect(load).toHaveBeenCalledTimes(1);
    // Idle data stopped the poll: nothing is scheduled.
    await advance(5_000);
    expect(load).toHaveBeenCalledTimes(1);

    current = "busy";
    await act(async () => {
      await result.current.refresh();
    });
    expect(load).toHaveBeenCalledTimes(2);
    await advance(1_000);
    expect(load).toHaveBeenCalledTimes(3);
    await advance(1_000);
    expect(load).toHaveBeenCalledTimes(4);
  });

  it("does not re-arm a stopped poll while the tab is hidden", async () => {
    vi.spyOn(Document.prototype, "visibilityState", "get").mockReturnValue("hidden");
    const load = vi.fn(() => Promise.resolve("busy"));
    const { result } = renderHook(() =>
      useResource(load, (d) => (d === "busy" ? 1_000 : null)),
    );
    await flush();
    // The initial load still runs; only the timer is gated on visibility.
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      await result.current.refresh();
    });
    expect(load).toHaveBeenCalledTimes(2);
    await advance(10_000);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("keeps an armed poll's cadence untouched by refresh", async () => {
    const load = vi.fn(() => Promise.resolve("x"));
    const { result } = renderHook(() => useResource(load, 1_000));
    await flush();
    await advance(1_000);
    expect(load).toHaveBeenCalledTimes(2);

    await act(async () => {
      await result.current.refresh();
    });
    expect(load).toHaveBeenCalledTimes(3);
    // One poll per interval — refresh must not arm a second timer.
    await advance(1_000);
    expect(load).toHaveBeenCalledTimes(4);
    await advance(1_000);
    expect(load).toHaveBeenCalledTimes(5);
  });
});
