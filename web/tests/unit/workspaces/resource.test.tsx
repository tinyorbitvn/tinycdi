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

// FIX-IDLE — the loader's `background` flag marks which reads may slide
// the portal idle window: scheduled ticks are background (the server
// peeks instead of touching last_seen_at) while mount loads, manual
// refresh and the return-to-visible reload are real activity.
describe("useResource background-poll marker", () => {
  it("marks timer ticks background; mount and refresh stay foreground", async () => {
    const load = vi.fn((_background: boolean) => Promise.resolve("x"));
    const { result } = renderHook(() => useResource(load, 1_000));
    await flush();
    expect(load).toHaveBeenLastCalledWith(false);

    await advance(1_000);
    expect(load).toHaveBeenLastCalledWith(true);
    await advance(1_000);
    expect(load).toHaveBeenLastCalledWith(true);

    await act(async () => {
      await result.current.refresh();
    });
    expect(load).toHaveBeenLastCalledWith(false);
  });

  it("treats the return-to-visible reload as activity", async () => {
    const visibility = vi
      .spyOn(Document.prototype, "visibilityState", "get")
      .mockReturnValue("hidden");
    const load = vi.fn((_background: boolean) => Promise.resolve("x"));
    renderHook(() => useResource(load, 1_000));
    await flush();
    expect(load).toHaveBeenLastCalledWith(false);
    // Hidden: the timer never fires.
    await advance(5_000);
    expect(load).toHaveBeenCalledTimes(1);

    visibility.mockReturnValue("visible");
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(load).toHaveBeenLastCalledWith(false);
  });
});
