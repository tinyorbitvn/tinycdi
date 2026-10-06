// FIX-IDLE — the portal's read surface is passive server-side, so the
// idle window only extends on real user interaction: POST
// /v1/session:touch is fired by pointer, key and route events (throttled
// to one beat per SESSION_TOUCH_INTERVAL_MS) and never by timer-driven
// polls or background fetches.
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createApi, setCsrfToken } from "../../../src/api/client";
import { navigate } from "../../../src/lib/router";
import {
  SESSION_TOUCH_INTERVAL_MS,
  useActivityTouch,
} from "../../../src/session/activity";
import { useResource } from "../../../src/workspaces/resource";

const flush = () => act(async () => {});
const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
const fire = (type: string) =>
  act(() => {
    window.dispatchEvent(new Event(type));
  });

function recordingClient(calls: string[]) {
  const fetchImpl = (async (input: RequestInfo | URL) => {
    const req = new Request(input);
    calls.push(`${req.method} ${new URL(req.url).pathname}`);
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  return createApi(fetchImpl);
}

const touches = (calls: string[]) => calls.filter((c) => c === "POST /v1/session:touch");

beforeEach(() => {
  vi.useFakeTimers();
  setCsrfToken("csrf-test");
  window.history.pushState(null, "", "/");
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useActivityTouch (FIX-IDLE)", () => {
  it("timer polls never send the beat", async () => {
    const calls: string[] = [];
    // The beat hook and a generic interval poll live in separate roots:
    // combining useSyncExternalStore (usePathname) with useResource's
    // timers under fake timers deadlocks the test runner — the poll is
    // exercised by the same client, which is what matters.
    renderHook(() => useActivityTouch(recordingClient(calls)));
    const load = vi.fn(() => Promise.resolve("x"));
    renderHook(() => useResource(load, 1_000));
    await flush();

    await advance(5_000); // five poll ticks — none may touch
    expect(load.mock.calls.length).toBeGreaterThan(1);
    expect(touches(calls)).toHaveLength(0);
  });

  it("pointer and key events beat, throttled to the interval", async () => {
    const calls: string[] = [];
    renderHook(() => useActivityTouch(recordingClient(calls)));
    await flush();

    fire("pointerdown");
    fire("keydown"); // collapses into the same window
    await flush();
    expect(touches(calls)).toHaveLength(1);

    await advance(SESSION_TOUCH_INTERVAL_MS - 1_000);
    fire("pointerdown");
    await flush();
    expect(touches(calls)).toHaveLength(1);

    await advance(1_001);
    fire("keydown");
    await flush();
    expect(touches(calls)).toHaveLength(2);
  });

  it("a route change beats", async () => {
    const calls: string[] = [];
    renderHook(() => useActivityTouch(recordingClient(calls)));
    await flush();

    act(() => navigate("/workspaces"));
    await flush();
    expect(touches(calls)).toHaveLength(1);
  });
});
